import {useEffect,useState,type FormEvent} from 'react'
import {api} from './api'
import {displayDate} from './GeneralPreferences'

export type DiscoveryStatus = {
  state:string
  plan?:{id:string;source:string;scope:string;next_at_unix_ms:number}
  last_run?:{observation_count?:number}
}
type Candidate={id:string;source:string;scope:string;serial?:string;address:string;seen_at:number;state:string;asset?:string}

const states:Record<string,string> = {
  no_active_plan:'没有启用的发现计划',waiting_for_result:'等待运行结果',
  no_observations:'本次运行没有观测',observations_available:'已有观测',
  run_failed:'最近一次运行失败',run_skipped:'最近一次运行已跳过',
}

export function DiscoveryPanel({status,refresh}:{status:DiscoveryStatus;refresh:()=>void}) {
  const [candidates,setCandidates]=useState<Candidate[]>()
  const [source,setSource]=useState(status.plan?.source??'')
  const [scope,setScope]=useState(status.plan?.scope??'')
  const [period,setPeriod]=useState(3600)
  const [grant,setGrant]=useState<{source:string;scope:string;expiry:number}>()
  const [admitting,setAdmitting]=useState<Candidate>()
  const [error,setError]=useState('')
  const [busy,setBusy]=useState(false)
  const [version,setVersion]=useState(0)

  useEffect(()=>{
    let active=true
    api<Candidate[]>('/domain/discovery/candidates').then(rows=>{if(active)setCandidates(rows)})
      .catch(e=>{if(active)setError(errorText(e))})
    return()=>{active=false}
  },[version])

  function errorText(error:unknown) {return error instanceof Error?error.message:'发现操作未完成'}
  async function execute(action:()=>Promise<unknown>) {
    if (busy) return
    setError('')
    setBusy(true)
    try {await action();refresh();setVersion(value=>value+1)}
    catch(e){setError(errorText(e))}
    finally{setBusy(false)}
  }
  function grantAccess() {
    const expiry=Date.now()+24*3600_000
    void execute(async()=>{
      await api('/domain/discovery/authorizations',{source,scope,expires_at_unix_ms:expiry})
      setGrant({source,scope,expiry})
    })
  }
  function publish(event:FormEvent) {
    event.preventDefault()
    if(grant?.source!==source||grant.scope!==scope||grant.expiry<=Date.now())return
    void execute(()=>api('/domain/discovery/plans',{
      id:'discovery-'+crypto.randomUUID().replaceAll('-',''),source,scope,period_seconds:period,first_at_unix_ms:Date.now()+60_000,
    }))
  }
  function admit(event:FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if(!admitting?.serial)return
    const fields=new FormData(event.currentTarget)
    const row=admitting
    void execute(async()=>{
      await api('/domain/discovery/candidates/'+encodeURIComponent(row.id)+'/admit',{
        confirmed_serial:row.serial,expected_seen_at:row.seen_at,owner:fields.get('owner'),
        location:fields.get('location'),role:fields.get('role'),
      })
      setAdmitting(undefined)
    })
  }

  return <section className="discovery-panel" aria-label="资产发现与定时计划">
    <header><div><strong>{states[status.state]??status.state}</strong>
      {status.last_run?.observation_count!==undefined&&<small> · {status.last_run.observation_count} 条观测</small>}</div>
      <button type="button" className="outline" onClick={()=>{refresh();setVersion(value=>value+1)}}>刷新</button>
    </header>
    {status.plan ? <div className="discovery-plan">
      <p>{status.plan.source} · {status.plan.scope} · 下次 {displayDate(status.plan.next_at_unix_ms,{month:'numeric',day:'numeric',hour:'2-digit',minute:'2-digit'})}</p>
      <button type="button" className="outline" disabled={busy} onClick={()=>void execute(()=>api('/domain/discovery/plans/'+encodeURIComponent(status.plan!.id),undefined,'DELETE'))}>停止计划</button>
    </div> : <form className="discovery-plan" onSubmit={publish}>
      <label>发现来源<input required value={source} onChange={e=>setSource(e.currentTarget.value)} maxLength={100}/></label>
      <label>发现范围<input required value={scope} onChange={e=>setScope(e.currentTarget.value)} maxLength={100}/></label>
      <label>周期（秒）<input required type="number" min={60} max={604800} value={period} onChange={e=>setPeriod(Number(e.currentTarget.value))}/></label>
      <button type="button" className="outline" disabled={busy||!source||!scope} onClick={grantAccess}>授权 24 小时</button>
      <button type="submit" className="primary" disabled={busy||grant?.source!==source||grant.scope!==scope}>发布计划</button>
    </form>}
    {candidates&&candidates.length>0&&<ul className="discovery-candidates">
      {candidates.map(row=><li key={row.id}><span><strong>{row.serial||row.address}</strong><small>{row.address} · {row.scope}</small></span>
        {row.state==='admitted'?<span className="muted">已纳管</span>:<button type="button" className="outline" disabled={!row.serial||busy} onClick={()=>setAdmitting(row)}>核对并纳管</button>}
      </li>)}
    </ul>}
    {admitting&&<form className="discovery-plan" onSubmit={admit}>
      <strong>{admitting.serial} · {admitting.address}</strong>
      <label>责任人<input name="owner" required maxLength={256}/></label>
      <label>位置<input name="location" required maxLength={256}/></label>
      <label>角色<input name="role" required maxLength={256}/></label>
      <button type="button" className="outline" onClick={()=>setAdmitting(undefined)}>取消</button>
      <button type="submit" className="primary" disabled={busy}>确认纳管</button>
    </form>}
    {error&&<p className="error" role="alert">{error}</p>}
  </section>
}
