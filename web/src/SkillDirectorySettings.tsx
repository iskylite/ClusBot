import {useCallback,useEffect,useRef,useState,type FormEvent} from 'react'
import {api,APIError} from './api'
import {BookOpen,ChevronRight,Copy,Pause,Plus,Upload} from 'lucide-react'

import {SkillDocumentImport,type DirectorySkill as Skill} from './SkillDocumentImport'
import './ConnectionSkillSettings.css'

type Proposal={skill:Pick<Skill,'skill_id'|'version'>}
type Draft={scope:string;instructions:string;revision:number}
const versionKey=(v:Pick<Skill,'skill_id'|'version'>)=>v.skill_id+':'+v.version
const empty:Draft={scope:'',instructions:'',revision:0}
const message=(e:unknown)=>e instanceof Error?e.message:'请求未完成'
const chars=(s:string)=>Array.from(s).length

export function SkillDirectorySettings({openProposals}:{openProposals:()=>void}){
  const [list,setList]=useState<Skill[]>()
  const [proposalKeys,setProposalKeys]=useState<Set<string>>()
  const [draft,setDraft]=useState<Draft>()
  const [showImport,setShowImport]=useState(false)
  const [review,setReview]=useState<string>()
  const [fresh,setFresh]=useState(false)
  const [busy,setBusy]=useState(false)
  const [notice,setNotice]=useState('')
  const [error,setError]=useState('')
  const generation=useRef(0)
  const lock=useRef(false)
  const refresh=useCallback(async()=>{
    const token=++generation.current
    setFresh(false)
    const [skills,proposals]=await Promise.allSettled([api<Skill[]>('/skills'),api<Proposal[]>('/skill-proposals')])
    if(token!==generation.current)return
    if(skills.status==='fulfilled'&&Array.isArray(skills.value)){
      setList(skills.value);setFresh(true)
    } else setError(skills.status==='rejected'?message(skills.reason):'无法核对 Skill 列表')
    setProposalKeys(proposals.status==='fulfilled'&&Array.isArray(proposals.value)?
      new Set(proposals.value.flatMap(p=>p.skill?[versionKey(p.skill)]:[])):undefined)
  },[])
  useEffect(()=>{void refresh();return()=>{generation.current++}},[refresh])

  const picked=list?.find(v=>versionKey(v)===review)??list?.[0]
  const knownProposal=Boolean(picked&&proposalKeys?.has(versionKey(picked)))
  const current=list?.find(v=>v.active&&v.skill_id===picked?.skill_id)
  const canActivate=(skill:Skill)=>Boolean(skill.instructions&&!skill.active&&(skill.approved_at||(!proposalKeys?.has(versionKey(skill))&&skill.base_revision===skill.revision)))
  const canEnable=Boolean(picked&&canActivate(picked))
  const scopeDraft=draft&&list?.find(v=>v.scope_label===draft.scope.trim())
  const staleDraft=Boolean(draft&&scopeDraft&&draft.revision!==scopeDraft.revision)
  async function action(go:()=>Promise<void>,done:string){
    if(!fresh||lock.current)return
    lock.current=true;generation.current++;setBusy(true);setNotice('');setError('')
    try{await go();setNotice(done);await refresh()}
    catch(e){
      setError(e instanceof APIError&&e.status===409?
        '版本、来源或审批状态已变化，未自动重试。已保留输入，请核对最新状态。':message(e))
      await refresh()
    } finally {lock.current=false;setBusy(false)}
  }
  function submit(e:FormEvent) {
    e.preventDefault()
    if(!draft||staleDraft)return
    const scope_label=draft.scope.trim(), instructions=draft.instructions.trim()
    if(!scope_label||!instructions||chars(scope_label)>40||chars(instructions)>4000){
      setError('请填写 1–40 字的工作范围与 1–4000 字的方法说明。');return
    }
    void action(async()=>{
      const version=await api<Skill>('/skills',{scope_label,instructions,revision:draft.revision})
      if(!version?.skill_id||version.scope_label!==scope_label||!version.version)throw new Error('未收到有效草稿回执，内容仍在。')
      setDraft(undefined);setReview(versionKey(version))
    },'文字草稿已保存，未启用或执行。')
  }
  function enable(skill:Skill){
    if(!canActivate(skill))return
    void action(()=>api('/skills/'+encodeURIComponent(skill.skill_id)+'/activate',{version:skill.version,revision:skill.revision,sha256:skill.sha256}),
      '服务器已确认启用该版本。未自动设定会话绑定或执行。')
  }
  function disable(skill:Skill){
    if(!skill.active)return
    void action(()=>api('/skills/'+encodeURIComponent(skill.skill_id)+'/disable',{revision:skill.revision}),
      '服务器已确认停用。该版本不会再成为新的选用项。')
  }
  return <section id="skill-directory" className="skill-directory" aria-labelledby="skill-directory-title">
    <div className="skill-list-panel settings-split-panel">
      <header className="settings-split-heading"><h2 id="skill-directory-title">共享 Skill 目录{list&&` (${list.length})`}</h2>
        <button type="button" className="primary" aria-expanded={showImport} onClick={()=>setShowImport(!showImport)}><Upload size={14} aria-hidden="true"/>导入 SKILL.md</button></header>
      {!list&&!error&&<p role="status">正在读取 Skill 目录…</p>}
      {list?.length===0&&<p className="settings-split-empty">暂无 Skill，可新建文字草稿。</p>}
      {list&&list.length>0&&<ul className="skill-list">{list.map(v=><li key={versionKey(v)} className={picked&&versionKey(picked)===versionKey(v)?'selected':''}>
        <button type="button" className="skill-list-select" aria-pressed={Boolean(picked&&versionKey(picked)===versionKey(v))} onClick={()=>setReview(versionKey(v))}>
          <ChevronRight size={13} aria-hidden="true"/>
          <strong>{v.scope_label}</strong><span className="settings-tag">v{v.version}</span><span className="settings-tag">sha256:{v.sha256.slice(0,4)}</span>
        </button>
        <label className="settings-switch"><input type="checkbox" checked={v.active} aria-label={`${v.active?'停用':'启用'} ${v.scope_label} v${v.version}`}
          disabled={busy||!fresh||(!v.active&&!canActivate(v))} onChange={()=>{setReview(versionKey(v));v.active?disable(v):enable(v)}}/><span/></label>
      </li>)}</ul>}
      {list?.length===0&&<button type="button" className="text-button skill-new-draft" disabled={!fresh||busy||Boolean(draft)} onClick={()=>{setShowImport(false);setDraft(empty)}}><Plus size={14} aria-hidden="true"/>新建文字草稿</button>}
    </div>
    <div className="skill-detail-panel settings-split-panel">
      <SkillDocumentImport visible={showImport} skills={list} ready={fresh&&!busy} onClose={()=>setShowImport(false)}
        onRefresh={()=>void refresh()} onImported={v=>{setReview(versionKey(v));void refresh()}}/>
      {showImport&&!draft&&<div className="skill-draft-options">
        <button type="button" className="text-button" disabled={!fresh||busy} onClick={()=>{setShowImport(false);setDraft(empty)}}>新建文字草稿</button>
        {picked?.instructions&&<button type="button" className="text-button" disabled={!fresh||busy} onClick={()=>{setShowImport(false);setDraft({scope:picked.scope_label,instructions:picked.instructions,revision:picked.revision})}}>据此新建文字草稿</button>}
      </div>}
      {picked&&!showImport&&!draft&&<section className="skill-review" aria-labelledby="skill-review-title">
        <header className="skill-review-title"><BookOpen size={15} aria-hidden="true"/><h3 id="skill-review-title">{picked.scope_label}</h3><span className="settings-tag">v{picked.version}</span></header>
        <div className="skill-meta">
          <p>状态：{picked.active?'当前启用 v'+picked.version:picked.approved_at?'已批准，未启用':'尚未批准'}。{!picked.active&&(current?' 当前启用 v'+current.version+'。':' 当前未启用版本。')}</p>
          <p>来源：{picked.source?.kind==='skill_md_main_document'?'SKILL.md 主文档':knownProposal?'受审经验提案':'目录未标明来源'}。启用不执行 Skill、不授予权限。</p>
          {picked.source?.kind==='skill_md_main_document'&&<><p>{picked.source.name} · {picked.source.description}</p><p>源文件 SHA-256：<code className="skill-digest">{picked.source.source_sha256}</code>。仅主文档，未导入附带资源。</p></>}
          {picked.approved_at&&<p>批准时间：{new Date(picked.approved_at).toLocaleString('zh-CN')}</p>}
          <p>指纹：<code className="skill-digest" title={picked.sha256}>sha256:{picked.sha256.slice(0,4)}…{picked.sha256.slice(-4)}</code></p>
        </div>
        <h4>SKILL.md 正文全文</h4>
        {picked.instructions?<pre className="skill-instructions">{picked.instructions}</pre>:<p>原始来源不可用，无法复核正文。</p>}
        {knownProposal&&!picked.approved_at&&<button type="button" className="text-button" onClick={openProposals}>在经验提案中复核并批准</button>}
        {!picked.approved_at&&picked.base_revision!==picked.revision&&<p>此草稿基于旧版本，不能直接启用。可用正文新建一版后复核。</p>}
        <div className="skill-review-actions">
          <button type="button" className="outline" disabled={!picked.instructions} onClick={()=>{if(!navigator.clipboard){setError('复制失败，请手动选择正文。');return} void navigator.clipboard.writeText(picked.instructions).then(()=>setNotice('正文已复制。')).catch(()=>setError('复制失败，请手动选择正文。'))}}><Copy size={13} aria-hidden="true"/>复制正文</button>
          {canEnable&&<button type="button" className="outline" disabled={busy||!fresh} onClick={()=>enable(picked)}>启用 v{picked.version}</button>}
          {picked.active&&<button type="button" className="outline skill-disable" disabled={busy||!fresh} onClick={()=>disable(picked)}><Pause size={13} aria-hidden="true"/>停用此 Skill</button>}
        </div>
      </section>}
      {!picked&&!showImport&&!draft&&<p className="settings-split-empty">选择一个 Skill 查看正文。</p>}
    {draft&&<form className="settings-fields" aria-labelledby="skill-draft-title" onSubmit={submit}>
      <h3 id="skill-draft-title">文字草稿</h3>
      <label className="settings-field"><span>工作范围</span><input name="scope_label" required value={draft.scope} readOnly={busy}
        onChange={e=>{const scope=e.currentTarget.value;setDraft({...draft,scope,revision:list?.find(v=>v.scope_label===scope.trim())?.revision??0})}}/></label>
      <label className="settings-field"><span>方法说明</span><textarea name="instructions" required rows={5} value={draft.instructions} readOnly={busy}
        onChange={e=>setDraft({...draft,instructions:e.currentTarget.value})}/><small>最多 4000 字。不输入密码、令牌等秘密。</small></label>
      {staleDraft&&<p role="status">工作范围版本已变化。已保留输入，请先复核当前版本，再选择用新版作为草稿基础。</p>}
      <div className="settings-actions">
        <button type="submit" className="primary" disabled={busy||!fresh||staleDraft}>保存文字草稿</button>
        {staleDraft&&scopeDraft&&<button type="button" className="text-button" disabled={busy||!fresh}
          onClick={()=>setDraft({...draft,revision:scopeDraft.revision})}>沿用输入，改用当前版本</button>}
        <button type="button" className="text-button" disabled={busy} onClick={()=>setDraft(undefined)}>关闭草稿</button>
      </div>
    </form>}
    {busy&&<p role="status">正在处理…</p>}{notice&&<p role="status">{notice}</p>}
    {error&&<p role="alert" className="error">{error} <button type="button" className="text-button" disabled={busy} onClick={()=>{setError('');void refresh()}}>重新读取</button></p>}
    </div>
  </section>
}
