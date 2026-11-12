import {useEffect,useState} from 'react'
import {ArrowLeft} from 'lucide-react'
import {api,type SkillProposal,type SkillReplay} from './api'

type CaseContext = {id:string;scope_label:string;goal:string}
type Report = {case_contexts:CaseContext[];results:({case_id:string;answer?:string}&Record<string,unknown>)[]}&Record<string,unknown>

const label = {pending:'待确认',active:'已生效',historical:'历史版本',stale:'基准版本已变更',unavailable:'来源不可用'}
const errorText = (error:unknown) => error instanceof Error?error.message:'操作未完成'
const targetKey = (c:CaseContext) => JSON.stringify([c.scope_label,c.goal])
const proposalVersion = (p:SkillProposal) => ({version:p.skill.version,revision:p.skill.revision,sha256:p.skill.sha256})
const replayStatus:Record<string,string> = {timeout:'超时',output_limit:'输出超限',model_error:'模型请求失败',empty_output:'无输出'}

function ReplaySummary({report}:{report:SkillReplay}) {
  return <div>
    <p><strong>{report.passed?'正文重放通过':'正文重放未通过'}</strong> · {report.model_config.model}</p>
    {(['current','candidate'] as const).map(arm=>{
      const metrics=report.metrics[arm]
      const tokens=(count:number,covered:number)=>covered===0?'未报告':`${count}（${covered}/${metrics.cases} 项有用量）`
      return <p key={arm}>{arm==='current'?'当前版':'候选版'} · 成功 {metrics.successful}/{metrics.cases}（{Math.round(metrics.success_rate*100)}%），
        完成 {metrics.completed}/{metrics.cases}，错误复用 {metrics.wrong_reuse_checked===0?'无可评分用例':`${metrics.wrong_reuse_count}/${metrics.wrong_reuse_checked}`}。
        输入 Token {tokens(metrics.input_tokens_known_total,metrics.input_usage_reported)}，输出 Token {tokens(metrics.output_tokens_known_total,metrics.output_usage_reported)}，
        请求耗时 {(metrics.duration_ms/1000).toFixed(1)} 秒。</p>
    })}
    <details><summary>逐项重放结果</summary>
      <ul>{report.results.map(result=><li key={result.case_id+'-'+result.policy}>
        {result.case_id} · {result.policy==='current'?'当前版':'候选版'} · {result.status==='completed'
          ? result.success?'成功':result.wrong_reuse?'错误复用':'未满足成功断言'
          : replayStatus[result.status]??'未完成'}
      </li>)}</ul>
    </details>
  </div>
}

function ReplayControls({proposal,busy,replay}:{proposal:SkillProposal;busy:boolean;replay:(suite:unknown)=>Promise<void>}) {
  const [file,setFile]=useState<File>()
  const [running,setRunning]=useState(false)
  const [error,setError]=useState('')

  async function run() {
    if(!file||busy||running)return
    setError('')
    setRunning(true)
    try {
      const suite=JSON.parse(await file.text()) as {cases?:CaseContext[];holdout_task_ids?:string[]}
      if(!Array.isArray(suite?.cases))throw new Error('请选择含用例的 JSON 文件。')
      const cases=suite.cases.filter(c=>c?.scope_label?.trim()===proposal.skill.scope_label && c?.goal?.trim()===proposal.goal)
      if(cases.length===0)throw new Error('文件里没有与此提案范围和目标匹配的用例。')
      await replay({cases,holdout_task_ids:suite.holdout_task_ids})
    } catch(e) {setError(errorText(e))}
    finally {setRunning(false)}
  }

  return <div className="memory-form">
    <label>正文重放用例<input type="file" accept=".json,application/json" disabled={busy||running}
      onChange={event=>{setFile(event.target.files?.[0]);setError('')}}/></label>
    <div><button className="outline" disabled={!file||busy||running} onClick={()=>void run()}>运行正文重放</button></div>
    {running&&<p className="muted" role="status">正在重放当前版和候选版…</p>}
    {error&&<p className="error" role="alert">{error}</p>}
  </div>
}

export function SkillProposalsPage({navigate,onChange}:{navigate:(path:string)=>void;onChange:()=>void}) {
  const [items,setItems]=useState<SkillProposal[]>()
  const [report,setReport]=useState<Report>()
  const [selected,setSelected]=useState('')
  const [error,setError]=useState('')
  const [busy,setBusy]=useState(false)
  const targets=[...new Map(report?.case_contexts.filter(c=>c.scope_label).map(c=>[targetKey(c),c])??[]).values()]

  async function reload() {
    setItems(await api<SkillProposal[]>('/skill-proposals'))
  }
  useEffect(()=>{void reload().catch(e=>setError(errorText(e)))},[])

  async function chooseReport(file?:File) {
    setError('')
    setReport(undefined)
    setSelected('')
    if(!file)return
    try {
      const value=JSON.parse(await file.text()) as Report
      if(!Array.isArray(value.case_contexts)||!Array.isArray(value.results)||
        !value.case_contexts.every(c=>c && typeof c.scope_label==='string' && typeof c.goal==='string' && typeof c.id==='string')||
        !value.results.every(r=>r && typeof r.case_id==='string')) {
        throw new Error('请使用含用例范围和目标的隔离评测报告。')
      }
      setReport(value)
      const first=value.case_contexts.find(c=>c.scope_label)
      if(first)setSelected(targetKey(first))
    } catch(e) {setError(errorText(e))}
  }

  async function act(path:string, input:unknown) {
    if(busy)return
    setError('')
    setBusy(true)
    try {
      await api(path,input)
      await reload()
      onChange()
    } catch(e) {
      setError(errorText(e))
      await reload().catch(()=>undefined)
    } finally {setBusy(false)}
  }

  function propose() {
    const target=targets.find(c=>targetKey(c)===selected)
    if(!target||!report)return
    const contexts=report.case_contexts.filter(c=>targetKey(c)===selected)
    const ids=new Set(contexts.map(c=>c.id))
    const results=report.results.filter(r=>ids.has(r.case_id)).map(({answer:_,...r})=>r)
    void act('/skill-proposals',{scope_label:target.scope_label,goal:target.goal,report:{...report,case_contexts:contexts,results}})
  }

  return <main className="main memory-list">
    <button type="button" className="text-button back" onClick={()=>navigate('/memory')}><ArrowLeft size={16}/>个人记忆</button>
    <header className="page-heading"><h1>Skill 提案</h1></header>
    <section className="work-goal memory-form">
      <label>评测报告<input type="file" accept=".json,application/json" disabled={busy}
        onChange={event=>void chooseReport(event.target.files?.[0])}/></label>
      {targets.length>0 && <label>适用目标<select value={selected} disabled={busy} onChange={event=>setSelected(event.target.value)}>
        {targets.map(c=><option key={targetKey(c)} value={targetKey(c)}>{c.scope_label} · {c.goal}</option>)}
      </select></label>}
      <div><button className="outline" disabled={!selected||busy} onClick={propose}>生成提案</button></div>
    </section>
    {error && <p className="error" role="alert">{error}</p>}
    {items?.map(p=><section className="work-goal" key={p.id}>
      <h2>{p.skill.scope_label} · {p.goal}</h2>
      <p className="muted">{p.status==='pending'&&!p.replay?.passed?'待重放通过':label[p.status]} · {p.base_version===null?'无基准版本':'基准 v'+p.base_version} → v{p.skill.version}</p>
      <p>经验策略评测完整配对 {p.evidence.paired_cases} 项，现行策略成功 {p.evidence.current_successful} 项，候选策略成功 {p.evidence.candidate_successful} 项。</p>
      {!!p.evidence.gains?.length && <p>改善用例 · {p.evidence.gains.join('、')}</p>}
      {!!p.evidence.regressions?.length && <p>退步用例 · {p.evidence.regressions.join('、')}</p>}
      {p.evidence.incomplete_pairs>0 && <p>未完整配对 {p.evidence.incomplete_pairs} 项</p>}
      {p.evidence.reuse_checked>0 && <p>错误复用 · 现行 {p.evidence.current_wrong_reuse}/{p.evidence.reuse_checked}，候选 {p.evidence.candidate_wrong_reuse}/{p.evidence.reuse_checked}</p>}
      {p.evidence.token_pairs>0 && <p>候选组 Token 变化 · {p.evidence.candidate_token_delta}（{p.evidence.token_pairs} 个用量完整的配对）</p>}
      {p.skill.instructions && <details open={p.status==='pending'}>
        <summary>提案内容</summary>
        <p>{p.base_instructions && p.skill.instructions.startsWith(p.base_instructions)
          ? p.skill.instructions.slice(p.base_instructions.length).trimStart() : p.skill.instructions}</p>
        {p.base_instructions && <details><summary>原版本</summary><p>{p.base_instructions}</p></details>}
      </details>}
      {p.status!=='unavailable' && <div className="task-actions">
        {p.sources.map(src=>src.task_id && <button type="button" className="text-button" key={src.candidate_id}
          onClick={()=>navigate(src.artifact_id?'/artifacts/'+src.artifact_id+'?task_id='+src.task_id
            : '/tasks/'+src.task_id+'#message-'+src.message_id)}>{src.artifact_id?'查看验收来源':'查看纠正来源'}</button>)}
      </div>}
      {p.replay&&<ReplaySummary report={p.replay}/>}
      {p.status==='pending'&&<ReplayControls proposal={p} busy={busy} replay={suite=>act('/skill-proposals/'+p.id+'/replay',{...proposalVersion(p),suite})}/>}
      <div className="task-actions">
        {p.status==='pending' && p.replay?.passed && <button className="primary" disabled={busy} onClick={()=>void act('/skill-proposals/'+p.id+'/approve',
          {...proposalVersion(p),replay_id:p.replay?.id})}>确认生效 v{p.skill.version}</button>}
        {p.skill.active && p.base_version!==null && <button className="outline" disabled={busy} onClick={()=>void act('/skills/'+p.skill.skill_id+'/activate',
          {version:p.base_version,revision:p.skill.revision,sha256:p.base_sha256})}>回滚到 v{p.base_version}</button>}
        {p.skill.active && p.base_version===null && <button className="outline" disabled={busy} onClick={()=>void act('/skills/'+p.skill.skill_id+'/disable',
          {revision:p.skill.revision})}>停用 v{p.skill.version}</button>}
      </div>
    </section>)}
  </main>
}
