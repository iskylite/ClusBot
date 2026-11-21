import {useCallback,useEffect,useId,useRef,useState,type FormEvent} from 'react'
import {api,APIError,type ModelProtocol,type ModelAttributes} from './api'

type Profile={id:string;name:string;protocol:ModelProtocol;base_url:string;model_id:string;revision:number;attributes?:ModelAttributes;credential_configured:boolean}
type Role={role:string;revision:number;source:string;status:string;profile_id?:string;profile_revision?:number;egress_fields?:string[]}
type RoleSpec={slug:string;title:string;shared:string}
const profilesPath='/settings/model/profiles'
const specs:RoleSpec[]=[
  {slug:'benchmark-admission',title:'基准受理',shared:'用户提交文字、已发布目标、可匹配的方案与版本'},
  {slug:'benchmark-revision',title:'基准方案修订',shared:'用户提交文字、可编辑测项和参数名'},
  {slug:'benchmark-execution',title:'基准执行建议',shared:'用户提交文字、待派发测项 ID 与顺序'},
]
const msg=(e:unknown)=>e instanceof Error?e.message:'请求未完成'
const statusNames:Record<string,string>={
  uses_existing_default:'使用默认模型',locally_configured:'专用档案已绑定（未代表远端可用）',
  profile_missing:'绑定的档案已不存在',profile_revision_changed:'档案版本已变更，需要重新绑定',
  profile_invalid:'档案配置不可用',credential_missing:'凭据不可用',credential_changed:'凭据已变化，需要重新绑定',
  demo_profile_disabled:'演示模式不启用专用档案',
}

export function ModelRolesSettings({refreshKey=0}:{refreshKey?:number}) {
  const [profiles,setProfiles]=useState<Profile[]>()
  const [error,setError]=useState('')
  const [editing,setEditing]=useState<Profile|true>()
  const [generation,setGeneration]=useState(0)
  const version=useRef(0)
  const load=useCallback(async()=>{
    const current=++version.current
    setError('')
    try {const list=await api<Profile[]>(profilesPath);if(version.current===current)setProfiles(Array.isArray(list)?list:[])}
    catch(e){if(version.current===current)setError(msg(e))}
  },[])
  useEffect(()=>{void load();return()=>{version.current++}},[load,refreshKey])
  return <>
    <section id="model-profiles" className="settings-card" aria-labelledby="model-profiles-title">
      <header className="settings-section-heading"><h2 id="model-profiles-title">模型档案</h2>
        {!editing&&profiles&&<button type="button" className="outline" onClick={()=>setEditing(true)}>新建档案</button>}
      </header>
      <p className="muted">档案保存一套模型连接。保存或试答不会改变默认模型，也不会自动绑定到角色。</p>
      {error?<p role="alert" className="error">{error} <button type="button" className="text-button" onClick={()=>void load()}>重新加载</button></p>:
        !profiles?<p role="status">正在读取档案…</p>:!profiles.length?<p className="muted">暂无模型档案。</p>:
        <ul className="model-settings-list">{profiles.map(profile=><li key={profile.id}>
          <div><strong>{profile.name}</strong><small>{profile.model_id} · 版本 {profile.revision} · {profile.credential_configured?'凭据可解析':'凭据状态由服务端校验'}</small></div>
          <button type="button" className="text-button" disabled={Boolean(editing)} onClick={()=>setEditing(profile)}>编辑或试答</button>
        </li>)}</ul>}
      {editing&&<ProfileForm key={editing===true?'new':editing.id} profile={editing===true?undefined:editing} onClose={()=>setEditing(undefined)}
        onSaved={profile=>{setProfiles(prev=>[...(prev??[]).filter(p=>p.id!==profile.id),profile]);setGeneration(n=>n+1)}}
        onConflict={()=>void load()} />}
    </section>
    <section id="model-roles" className="settings-card" aria-labelledby="model-roles-title">
      <h2 id="model-roles-title">角色用哪一个模型</h2>
      <p className="muted">这里保留原有角色绑定。阶段策略或业务覆盖存在时，以生效预览为准。</p>
      {specs.map(spec=><RoleForm key={spec.slug} spec={spec} profiles={profiles} generation={generation+refreshKey} refreshProfiles={load}/>)}
    </section>
  </>
}

function ProfileForm({profile,onClose,onSaved,onConflict}:{
  profile?:Profile;onClose:()=>void;onSaved:(p:Profile)=>void;onConflict:()=>void
}) {
  const title=useId()
  const make=(p?:Profile)=>({name:p?.name??'',protocol:p?.protocol??'responses',base_url:p?.base_url??'',model_id:p?.model_id??'',attributes:p?.attributes??{},keyEnv:''})
  const [draft,setDraft]=useState(()=>make(profile))
  const [stored,setStored]=useState(profile)
  const [busy,setBusy]=useState(false)
  const [error,setError]=useState('')
  const [notice,setNotice]=useState('')
  const lock=useRef(false)
  const dirty=!stored||JSON.stringify(draft)!==JSON.stringify(make(stored))
  function edit<K extends keyof typeof draft>(k:K,v:(typeof draft)[K]) {
    setDraft(d=>({...d,[k]:v,...(['protocol','base_url','model_id'].includes(k)?{attributes:{}}:{})}));setError('');setNotice('')
  }
  async function process(action:()=>Promise<void>){
    if(lock.current)return
    lock.current=true;setBusy(true);setNotice('');setError('')
    try{await action()}catch(e){setError(e instanceof APIError&&e.status===409?'服务端版本已改变，已保留输入。返回列表后打开最新版本核对，未覆盖现有设置。':msg(e));if(e instanceof APIError&&e.status===409)onConflict()}
    finally{lock.current=false;setBusy(false)}
  }
  function submit(e:FormEvent){
    e.preventDefault()
    if(!dirty)return
    void process(async()=>{
      const {keyEnv,...values}=draft
      const path=stored?profilesPath+'/'+encodeURIComponent(stored.id):profilesPath
      const saved=await api<Profile>(path,{...values,revision:stored?.revision??0,...(keyEnv.trim()?{api_key_ref:'env:'+keyEnv.trim()}: {})},stored?'PUT':'POST')
      setStored(saved);setDraft(make(saved));setNotice('档案已保存。角色没有自动改绑。');onSaved(saved)
    })
  }
  function test(){
    if(!stored||dirty)return
    void process(async()=>{
      const result=await api<{duration_ms:number;revision:number}>(profilesPath+'/'+encodeURIComponent(stored.id)+'/test',{revision:stored.revision})
      if(result.revision!==stored.revision)throw new Error('试答回执版本无法核对')
      setNotice('此版本试答成功：'+result.duration_ms+' 毫秒。')
    })
  }
  return <form className="settings-fields" aria-labelledby={title} onSubmit={submit}>
    <h3 id={title}>{stored?'编辑档案':'新建档案'}</h3>
    <label className="settings-field"><span>档案名称</span><input name="name" required maxLength={60} value={draft.name} onChange={e=>edit('name',e.currentTarget.value)} readOnly={busy}/></label>
    <label className="settings-field"><span>协议</span><select name="protocol" value={draft.protocol} disabled={busy} onChange={e=>edit('protocol',e.currentTarget.value as ModelProtocol)}>
      <option value="responses">Responses</option><option value="chat_completions">Chat Completions</option></select></label>
    <label className="settings-field"><span>服务地址</span><input name="base_url" type="url" required value={draft.base_url} onChange={e=>edit('base_url',e.currentTarget.value)} readOnly={busy}/>
      <small>HTTPS，或使用回环 IP 的本地 HTTP。</small></label>
    <label className="settings-field"><span>模型 ID</span><input name="model_id" required maxLength={256} value={draft.model_id} onChange={e=>edit('model_id',e.currentTarget.value)} readOnly={busy}/></label>
    <label className="settings-field"><span>密钥环境变量名</span><input name="credential_env" autoComplete="off" required={!stored||draft.base_url.trim()!==stored.base_url}
      pattern="[A-Za-z_][A-Za-z0-9_]*" value={draft.keyEnv} onChange={e=>edit('keyEnv',e.currentTarget.value)} readOnly={busy}/>
      <small>只填写例如 CLUSBOT_MODEL_KEY 的变量名，不输入密钥值。同地址编辑留空则保留原引用。</small></label>
    {(['context_tokens','max_output_tokens'] as const).map(key=><label className="settings-field" key={key}>
      <span>{key==='context_tokens'?'上下文窗口（Token）':'最大输出长度（Token）'}</span>
      <input name={key} type="number" min={1} step={1} value={draft.attributes[key]??''} readOnly={busy} placeholder="未声明"
        onChange={e=>edit('attributes',{...draft.attributes,[key]:e.currentTarget.value?e.currentTarget.valueAsNumber:undefined})}/>
    </label>)}
    <div className="settings-actions">
      <button type="submit" className="primary" disabled={busy||!dirty}>保存档案</button>
      {stored&&<button type="button" className="outline" disabled={busy||dirty} onClick={test}>试答已保存版本</button>}
      <button type="button" className="text-button" disabled={busy} onClick={onClose}>返回档案列表</button>
    </div>
    {busy&&<p role="status">请求处理中…</p>}{notice&&<p role="status">{notice}</p>}{error&&<p role="alert" className="error">{error}</p>}
  </form>
}

function RoleForm({spec,profiles,generation,refreshProfiles}:{spec:RoleSpec;profiles?:Profile[];generation:number;refreshProfiles:()=>Promise<void>}) {
  const title=useId(), [role,setRole]=useState<Role>(), [choice,setChoice]=useState(''), [consentKey,setConsent]=useState('')
  const [error,setError]=useState(''),[notice,setNotice]=useState(''),[busy,setBusy]=useState(false)
  const active=useRef(0),lock=useRef(false),editingChoice=useRef(false),known=useRef<Role|undefined>(undefined)
  const path='/settings/model/roles/'+spec.slug
  const load=useCallback(async()=>{
    const request=++active.current
    try {
      const result=await api<Role>(path)
      if(!result||!Number.isInteger(result.revision))throw new Error('此角色的配置接口暂不可用')
      if(active.current!==request)return
      const old=known.current
      known.current=result
      setRole(result)
      if(!editingChoice.current){setChoice(result.profile_id??'');setConsent('')}
      else if(old&&(old.revision!==result.revision||old.profile_revision!==result.profile_revision||old.status!==result.status)){
        setConsent('');setNotice('服务端状态已变化，已保留你的选择，请核对后重新确认。')
      }
    }
    catch(e){if(active.current===request)setError(msg(e))}
  },[path])
  useEffect(()=>{void load();return()=>{active.current++}},[generation,load])
  const selected=profiles?.find(p=>p.id===choice)
  const selectedKey=selected?selected.id+':'+selected.revision:''
  const consent=Boolean(selectedKey&&consentKey===selectedKey)
  const unavailable=role?.profile_id&& !profiles?.some(p=>p.id===role.profile_id)
  function save(e:FormEvent) {
    e.preventDefault()
    if(!role||lock.current||choice&&(!selected||!consent))return
    const oldRevision=role.revision
    const body=selected
      ? {action:'bind_profile',revision:oldRevision,profile_id:selected.id,profile_revision:selected.revision,confirm_request_text_egress:true}
      : {action:'use_existing_default',revision:oldRevision}
    lock.current=true;active.current++;setBusy(true);setError('');setNotice('')
    void api<Role>(path,body,'PUT').then(result=>{if(!result||!Number.isInteger(result.revision))throw new Error('未收到有效角色保存回执');known.current=result;editingChoice.current=false;setRole(result);setChoice(result.profile_id??'');setConsent('');setNotice('角色配置已保存。')})
      .catch(e=>{if(e instanceof APIError&&e.status===409){setError('档案或角色版本已变化，未改绑。正在重新读取角色和档案，请核对后再提交。');void load();void refreshProfiles()}else setError(msg(e))})
      .finally(()=>{lock.current=false;setBusy(false)})
  }
  return <form className="settings-fields model-role-fields" aria-labelledby={title} onSubmit={save}>
    <h3 id={title}>{spec.title}</h3>
    {spec.slug==='benchmark-execution'&&<p className="muted">只选择生成建议的模型，不授予执行权限。重新绑定后，回到会话发起一条新建议。</p>}
    {!role&&!error&&<p role="status">正在读取角色…</p>}
    {role&&<>
      <p className="muted">{statusNames[role.status]??'状态待核对'}{role.profile_revision!==undefined&&' · 档案版本 '+role.profile_revision}</p>
      <label className="settings-field"><span>模型来源</span><select name="role_profile" value={choice} disabled={busy}
        onChange={e=>{editingChoice.current=true;setChoice(e.currentTarget.value);setConsent('');setNotice('');setError('')}}>
        <option value="">沿用默认模型</option>
        {unavailable&&<option value={role.profile_id} disabled>已绑定档案不可用</option>}
        {profiles?.map(p=><option key={p.id} value={p.id}>{p.name} · v{p.revision}{!p.credential_configured?' · 凭据状态待校验':''}</option>)}
      </select></label>
      {selected&&<label className="model-egress"><input type="checkbox" checked={consent} disabled={busy} onChange={e=>{editingChoice.current=true;setConsent(e.currentTarget.checked?selectedKey:'')}}/>
        <span>允许该角色将以下内容交由此档案的模型处理：{spec.shared}。</span></label>}
      <div className="settings-actions"><button type="submit" className="outline"
        disabled={busy||Boolean(choice&&(!selected||!consent))||(!choice&&!role.profile_id)}>保存角色配置</button></div>
    </>}
    {notice&&<p role="status">{notice}</p>}{error&&<p role="alert" className="error">{error} {!role&&<button type="button" className="text-button" onClick={()=>{setError('');void load()}}>重新加载</button>}</p>}
  </form>
}
