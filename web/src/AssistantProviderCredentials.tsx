import {useEffect,useId,useRef,useState,type FormEvent} from 'react'
import {ArrowLeft,Eye,EyeOff,KeyRound,ShieldCheck} from 'lucide-react'
import {api,APIError,type ModelProtocol} from './api'
import './assistant-credentials.css'

type AuthMode='api_key'|'environment'|'none'
type Provider={id:string;name:string;protocol:ModelProtocol;base_url:string;auth_mode:AuthMode;enabled:boolean;revision:number;credential_status:string}
type Probe={connection:string;authentication:string}
const endpoint='/settings/model/providers'
const choices:{value:AuthMode;label:string}[]=[
  {value:'api_key',label:'API Key'},{value:'environment',label:'环境变量'},{value:'none',label:'无凭据'},
]

export function AssistantProviderCredentials({navigate,providerID,providerRevision,requireSelection=false,onClose}:{
  navigate:(path:string)=>void;providerID?:string;providerRevision?:number;requireSelection?:boolean;onClose?:()=>void;
}){
  const [providers,setProviders]=useState<Provider[]>()
  const [selection,setSelection]=useState('')
  const [editing,setEditing]=useState(true)
  const [error,setError]=useState('')
  const provider=providers?.find(item=>item.id===selection)
  useEffect(()=>{
    let active=true
    api<Provider[]>(endpoint).then(items=>{
      if(active){
        const match=items.find(item=>item.id===providerID)
        const valid=match&&(providerRevision===undefined||match.revision===providerRevision)
        setProviders(items)
        setSelection(valid?match.id:!providerID&&!requireSelection&&items.length===1?items[0].id:'')
        if(providerID&&!valid)setError('提供方配置已变化，请重新选择并核对。')
      }
    }).catch(()=>{if(active)setError('无法读取提供方。')})
    return()=>{active=false}
  },[providerID,providerRevision,requireSelection])

  if(!editing)return null
  return <section className="assistant-credentials" aria-label="编辑凭据">
    {provider?<CredentialFields key={provider.id} provider={provider} back={()=>{if(onClose)onClose();else if(providers?.length===1)setEditing(false);else setSelection('')}}/>:<>
      <h3><KeyRound size={15}/>编辑凭据</h3>
      {!providers&&!error&&<p role="status">正在读取提供方…</p>}
      {error&&<p className="error" role="alert">{error}</p>}
      {providers?.length?<label>服务提供方 <select value={selection} onChange={event=>setSelection(event.target.value)}>
        <option value="">请选择</option>
        {providers.map(item=><option key={item.id} value={item.id}>{item.name}</option>)}
      </select></label>:providers&&<button type="button" className="outline" onClick={()=>navigate('/settings?tab=model')}>添加服务提供方</button>}
      {onClose&&<footer><button type="button" className="outline" onClick={onClose}><ArrowLeft size={13}/>返回</button></footer>}
    </>}
  </section>
}

function CredentialFields({provider,back}:{provider:Provider;back:()=>void}){
  const group=useId()
  const [current,setCurrent]=useState(provider)
  const [mode,setMode]=useState<AuthMode>(provider.auth_mode)
  const [visible,setVisible]=useState(false)
  const [busy,setBusy]=useState(false)
  const [notice,setNotice]=useState('')
  const [error,setError]=useState('')
  const [reloadRequired,setReloadRequired]=useState(false)
  const credential=useRef<HTMLInputElement>(null)
  const lock=useRef(false)
  async function verify(event:FormEvent<HTMLFormElement>){
    event.preventDefault()
    if(lock.current||reloadRequired)return
    const value=credential.current?.value.trim()??''
    if(credential.current)credential.current.value=''
    setVisible(false)
    setBusy(true);setNotice('');setError('');lock.current=true
    const path=endpoint+'/'+encodeURIComponent(current.id)
    let saved=false
    try{
      const item=await api<Provider>(path,{
        name:current.name,protocol:current.protocol,base_url:current.base_url,enabled:current.enabled,revision:current.revision,auth_mode:mode,
        ...(value&&mode==='api_key'?{api_key:value}:{}),
        ...(value&&mode==='environment'?{api_key_ref:'env:'+value}:{}),
      },'PUT')
      saved=true
      setCurrent(item)
      const probe=await api<Probe>(path+'/diagnose',{revision:item.revision})
      setNotice(probe.connection==='reachable'&&['not_rejected','not_used'].includes(probe.authentication)?
        '凭据已保存，连接验证通过。':'凭据已保存。连接或认证尚未通过诊断。')
    }catch(error){
      setError(saved?'凭据已保存，连接诊断未完成。':error instanceof APIError&&error.status===409?'提供方配置已变化，请从模型设置重新核对。':'未能保存凭据。')
      if(!saved&&error instanceof APIError&&error.status===409)setReloadRequired(true)
    }finally{setBusy(false);lock.current=false}
  }
  const needsCredential=mode!==current.auth_mode||current.credential_status==='missing'
  return <form onSubmit={verify} className="assistant-credential-form" aria-busy={busy}>
    <h3><KeyRound size={15}/>编辑凭据 · {current.name}</h3>
    <fieldset className="assistant-auth-options"><legend className="app-screen-reader">认证方式</legend>
      {choices.map(choice=><label key={choice.value}><input type="radio" name={group} value={choice.value} checked={choice.value===mode} disabled={busy}
        onChange={()=>{setMode(choice.value);setVisible(false);setNotice('')}}/><span>{choice.label}</span></label>)}
    </fieldset>
    {mode!=='none'&&<div className="assistant-credential-field">
      <input key={mode} ref={credential} type={mode==='api_key'&&!visible?'password':'text'}
        aria-label={mode==='api_key'?'API Key':'密钥环境变量'} autoComplete={mode==='api_key'?'new-password':'off'}
        spellCheck={false} required={needsCredential} maxLength={mode==='api_key'?4096:80} pattern={mode==='environment'?'[A-Za-z_][A-Za-z0-9_]*':undefined}
        placeholder={needsCredential?(mode==='api_key'?'输入 API Key':'输入环境变量名'):'留空以保留现有配置'} readOnly={busy}/>
      {mode==='api_key'&&<button type="button" className="icon-button" aria-label={visible?'隐藏密钥':'显示密钥'} aria-pressed={visible} onClick={()=>setVisible(!visible)}>{visible?<EyeOff size={14}/>:<Eye size={14}/>}</button>}
    </div>}
    {error&&<p className="error" role="alert">{error}</p>}
    {notice&&<p role="status">{notice}</p>}
    <footer><button type="button" className="outline" onClick={back} disabled={busy}><ArrowLeft size={13}/>返回</button>
      <button type="submit" className="primary" disabled={busy||reloadRequired}><ShieldCheck size={13}/>{busy?'正在保存并验证…':'保存并验证'}</button></footer>
  </form>
}
