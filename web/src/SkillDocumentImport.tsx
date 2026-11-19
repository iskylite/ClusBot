import {useId,useRef,useState,type FormEvent} from 'react'
import {api,APIError} from './api'

export type SkillDocumentSource={kind:string;name:string;description:string;source_sha256:string;resource_status:string;optional_unsupported?:string[]}
export type DirectorySkill={skill_id:string;scope_label:string;revision:number;version:number;base_revision:number;instructions:string;sha256:string;active:boolean;approved_at?:number;source?:SkillDocumentSource}
type Input={filename:'SKILL.md';document:string;scope_label:string;revision:number}
type Preview={source:SkillDocumentSource;body_sha256:string;instructions:string;scope_label:string;revision:number;preview_binding:string}
type Previewed={input:Input;preview:Preview}
const hash=(s?:string)=>Boolean(s&&/^[a-f0-9]{64}$/.test(s))

export async function readSkillMainFile(file:Pick<File,'name'|'size'|'arrayBuffer'>) {
  if(file.name!=='SKILL.md'||file.size>48*1024||!file.size)throw new Error('请选择名为 SKILL.md、大小不超过 48 KiB 的单个文件。')
  try {
    return new TextDecoder('utf-8',{fatal:true,ignoreBOM:true}).decode(await file.arrayBuffer())
  }catch{throw new Error('无法读取文件，请确认它是有效的 UTF-8 文本。')}
}

export function SkillDocumentImport({visible,skills,ready,onImported,onRefresh,onClose}:{
  visible:boolean;skills?:DirectorySkill[];ready:boolean;onImported:(v:DirectorySkill)=>void;onRefresh:()=>void;onClose:()=>void
}){
  const title=useId(),fileRef=useRef<HTMLInputElement>(null),lock=useRef(false)
  const [scope,setScope]=useState('')
  const [document,setDocument]=useState('')
  const [review,setReview]=useState<Previewed>()
  const [pending,setPending]=useState<Previewed>()
  const [busy,setBusy]=useState(false)
  const [notice,setNotice]=useState('')
  const [error,setError]=useState('')
  const scopeLabel=scope.trim()
  const revision=skills?.find(v=>v.scope_label===scopeLabel)?.revision??0
  const currentReview=review?.input.scope_label===scopeLabel&&review.input.revision===revision&&review.input.document===document?review:undefined
  const current= pending??currentReview
  function resetReview(){setReview(undefined);setError('');setNotice('')}
  async function read(file?:File){
    if(lock.current||pending)return
    resetReview();setDocument('')
    if(!file)return
    lock.current=true;setBusy(true)
    try{setDocument(await readSkillMainFile(file))}
    catch(e){setError(e instanceof Error?e.message:'无法读取所选文件');if(fileRef.current)fileRef.current.value=''}
    finally{lock.current=false;setBusy(false)}
  }
  async function preview(e:FormEvent){
    e.preventDefault()
    if(lock.current||pending||!ready||!document||!scopeLabel||Array.from(scopeLabel).length>40)return
    const input:Input={filename:'SKILL.md',document,scope_label:scopeLabel,revision}
    lock.current=true;setBusy(true);resetReview()
    try{
      const v=await api<Preview>('/skills/import-preview',input)
      if(v?.scope_label!==input.scope_label||v.revision!==input.revision||v.source?.kind!=='skill_md_main_document'||
          v.source?.resource_status!=='main_document_only'||!hash(v.source.source_sha256)||!hash(v.body_sha256)||!hash(v.preview_binding)||!v.instructions)
        throw new Error('未收到可核对的 Skill 预览。')
      setReview({input,preview:v})
    }catch(e){
      setError(e instanceof Error?e.message:'未收到预览')
      if(e instanceof APIError&&e.status===409)onRefresh()
    }finally{lock.current=false;setBusy(false)}
  }
  async function importDocument(record:Previewed){
    if(lock.current||(!pending&&(!ready||record!==currentReview)))return
    lock.current=true;setBusy(true);setError('');setNotice('');setPending(record)
    const {input,preview}=record
    try{
      const v=await api<DirectorySkill>('/skills/import',{...input,expected_source_sha256:preview.source.source_sha256,preview_binding:preview.preview_binding})
      if(!v?.skill_id||!Number.isSafeInteger(v.version)||v.version<1||v.scope_label!==input.scope_label||v.base_revision!==input.revision||
          v.source?.source_sha256!==preview.source.source_sha256||v.sha256!==preview.body_sha256)
        throw new Error('无法核对导入回执')
      setPending(undefined);setReview(undefined);setDocument('')
      if(fileRef.current)fileRef.current.value=''
      setNotice('已核对 v'+v.version+'。导入本身不启用；服务器返回时当前'+(v.active?'已启用。':'未启用。'))
      onImported(v)
    }catch(e){
      if(e instanceof APIError&&['skill_document_source_changed','invalid_skill_document','task_revision'].includes(e.code??'')){
        setPending(undefined);setReview(undefined);setError('文件或范围版本未通过核对。已保留文件内容，重新预览后再导入。');onRefresh()
      }else setError('暂未核实是否已导入。不会自动重发；手动核对会使用同一文件、范围、版本与预览。')
    } finally{lock.current=false;setBusy(false)}
  }
  return <section hidden={!visible} aria-labelledby={title}>
    <header className="settings-section-heading"><h3 id={title}>导入 SKILL.md 主文档</h3>
      <button type="button" className="text-button" disabled={busy} onClick={onClose}>收起</button></header>
    <p className="muted">只导入选中的主文件，不读目录、压缩包、脚本、链接或附带资源。导入不会运行、授权或绑定 Skill。</p>
    <form className="settings-fields" onSubmit={e=>void preview(e)} aria-labelledby={title}>
      <label className="settings-field"><span>工作范围</span><input name="scope_label" required value={scope}
        disabled={busy||Boolean(pending)} onChange={e=>{setScope(e.currentTarget.value);resetReview()}}/></label>
      <label className="settings-field"><span>选取 SKILL.md（UTF-8，最多 48 KiB）</span>
        <input ref={fileRef} name="skill_file" type="file" accept=".md,text/markdown" disabled={busy||Boolean(pending)}
          onChange={e=>void read(e.currentTarget.files?.[0])}/></label>
      {scopeLabel&&<p className="muted">工作范围修订：{ready?revision:'正在核对'}。</p>}
      {review&&!current&&<p role="status">范围修订已变化，旧预览不可用。</p>}
      {!pending&&<div className="settings-actions"><button type="submit" className="outline"
        disabled={busy||!ready||!document||!scopeLabel||Array.from(scopeLabel).length>40}>预览主文档</button></div>}
    </form>
    {current&&<section className="settings-fields" aria-label="服务端检查的导入预览">
      <h4>{current.preview.source.name}</h4>
      <p>{current.preview.source.description}</p>
      <p className="muted">{current.input.scope_label} · 范围修订 {current.input.revision}</p>
      <p className="muted">源文件 SHA-256：<code className="skill-digest">{current.preview.source.source_sha256}</code></p>
      <p className="muted">正文 SHA-256：<code className="skill-digest">{current.preview.body_sha256}</code></p>
      <p className="muted">不使用资源或工具权限。{Boolean(current.preview.source.optional_unsupported?.length)&&'未采用的字段：'+current.preview.source.optional_unsupported?.join('、')+'。'}</p>
      <p className="skill-instructions">{current.preview.instructions}</p>
      <div className="settings-actions"><button type="button" className="primary" disabled={busy||(!pending&&!ready)}
        onClick={()=>void importDocument(current)}>{pending?'核对并重试原导入':'确认导入为草稿'}</button></div>
    </section>}
    {busy&&<p role="status">正在读取或核对…</p>}{notice&&<p role="status">{notice}</p>}{error&&<p role="alert" className="error">{error}</p>}
  </section>
}
