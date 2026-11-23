import type {Preferences} from './GeneralPreferences'

export type PreferenceKey = Exclude<keyof Preferences, 'revision'>
type PreferenceValue = string | boolean
type PendingChange = {base:PreferenceValue; value:PreferenceValue; sequence:number}
type PendingChanges = Partial<Record<PreferenceKey, PendingChange>>
type PreferenceStatus = 'loading' | 'idle' | 'saving' | 'error' | 'conflict'
export type PreferenceSnapshot = {value?:Preferences; status:PreferenceStatus; error?:string; conflicts:PreferenceKey[]}
type PreferenceAPI = {load:()=>Promise<Preferences>; save:(value:Preferences)=>Promise<Preferences>; isConflict:(error:unknown)=>boolean}

const keys:PreferenceKey[] = ['language','timezone','appearance','reduce_motion','send_shortcut','desktop_notifications']
function validValue(key:PreferenceKey, value:unknown):value is PreferenceValue {
  if(key==='language') return value==='system'||value==='zh-CN'||value==='en-US'
  if(key==='appearance') return value==='system'||value==='light'||value==='dark'
  if(key==='send_shortcut') return value==='enter'||value==='mod_enter'
  if(key==='timezone') return typeof value==='string'&&value.length>0&&value.length<=128
  return typeof value==='boolean'
}
function validPreferences(value:unknown):value is Preferences {
  if(!value||typeof value!=='object')return false
  const record=value as Record<string,unknown>
  return Number.isInteger(record.revision)&&Number(record.revision)>=0&&keys.every(key=>validValue(key,record[key]))
}

// One controller lives for the workspace, not an individual settings tab. Each
// PUT is serialized and only locally changed fields are rebased after a 409.
export class PreferencesController {
  private confirmed?:Preferences
  private pending:PendingChanges = {}
  private conflicts:PreferenceKey[] = []
  private sequence = 0
  private status:PreferenceStatus = 'loading'
  private error?:string
  private running = false
  private stopped = false
  private listener?: (snapshot:PreferenceSnapshot)=>void

  constructor(private readonly api:PreferenceAPI, private readonly storage:Storage | undefined, private readonly storageKey:string) {
    try {
      const saved=JSON.parse(storage?.getItem(storageKey)??'null') as {confirmed?:unknown; pending?:unknown}|null
      if(saved&&validPreferences(saved.confirmed)&&saved.pending&&typeof saved.pending==='object') {
        this.confirmed=saved.confirmed
        const pending=saved.pending as Record<string,unknown>
        for(const key of keys) {
          const entry=pending[key] as {base?:unknown;value?:unknown}|undefined
          if(entry&&validValue(key,entry.base)&&validValue(key,entry.value)) {
            this.pending[key]={base:entry.base,value:entry.value,sequence:++this.sequence}
          }
        }
      }
    } catch { /* Storage can be unavailable; in-memory edits still work. */ }
  }

  snapshot():PreferenceSnapshot {
    let value=this.confirmed ? {...this.confirmed} : undefined
    if(value)for(const key of keys) {
      const entry=this.pending[key]
      if(entry)value={...value,[key]:entry.value}
    }
    return {value,status:this.status,error:this.error,conflicts:[...this.conflicts]}
  }
  subscribe(listener:(snapshot:PreferenceSnapshot)=>void) {
    this.listener=listener
    listener(this.snapshot())
    return ()=>{if(this.listener===listener)this.listener=undefined}
  }
  start() {this.stopped=false;void this.sync(true)}
  stop() {this.stopped=true}
  retry() {if(!this.running){this.conflicts=[];void this.sync(true)}}

  update<K extends PreferenceKey>(key:K, value:Preferences[K]) {
    if(!this.confirmed||!validValue(key,value))return
    const existing=this.pending[key]
    if(existing?.value===value||(!existing&&this.confirmed[key]===value))return
    this.pending[key]={base:existing?.base??this.confirmed[key],value,sequence:++this.sequence}
    // If there is no in-flight write, reverting to the confirmed value needs no PUT.
    if(!this.running&&this.confirmed[key]===value) {
      delete this.pending[key]
      this.conflicts=this.conflicts.filter(conflict=>conflict!==key)
    }
    this.persist()
    if(this.status==='conflict'&&this.conflicts.length>0) {this.publish();return}
    if(this.running) {this.publish();return}
    const refresh=this.status==='error'
    this.status='idle';this.error=undefined
    this.publish()
    void this.sync(refresh)
  }

  resolveConflicts(keepMine:boolean) {
    if(!this.confirmed)return
    for(const key of this.conflicts) {
      const entry=this.pending[key]
      if(!entry)continue
      if(keepMine)entry.base=this.confirmed[key]
      else delete this.pending[key]
    }
    this.conflicts=[];this.error=undefined;this.status='idle'
    this.persist();this.publish()
    void this.sync(false)
  }

  private publish() {this.listener?.(this.snapshot())}
  private persist() {
    try {
      if(Object.keys(this.pending).length===0)this.storage?.removeItem(this.storageKey)
      else this.storage?.setItem(this.storageKey,JSON.stringify({confirmed:this.confirmed,pending:this.pending}))
    } catch { /* The active workspace still retains the edits. */ }
  }
  private reconcile(latest:Preferences) {
    this.conflicts=[]
    for(const key of keys) {
      const entry=this.pending[key]
      if(!entry)continue
      if(latest[key]===entry.value)delete this.pending[key]
      else if(latest[key]!==entry.base)this.conflicts.push(key)
    }
    this.confirmed=latest
    this.persist()
  }
  private acknowledge(result:Preferences, sent:PendingChanges) {
    this.confirmed=result
    for(const key of keys) {
      const current=this.pending[key]
      if(!current)continue
      if(current.value===result[key]||sent[key]?.sequence===current.sequence)delete this.pending[key]
      else if(sent[key])current.base=result[key]
    }
    this.persist()
  }
  private async sync(refresh:boolean) {
    if(this.running||this.stopped)return
    this.running=true;this.error=undefined
    try {
      if(refresh||!this.confirmed) {
        this.status='loading';this.publish()
        this.reconcile(await this.api.load())
      }
      while(!this.stopped&&this.confirmed&&Object.keys(this.pending).length>0) {
        if(this.conflicts.length>0) {this.status='conflict';this.publish();return}
        const sent:PendingChanges={}
        let payload={...this.confirmed}
        for(const key of keys) {
          const entry=this.pending[key]
          if(entry) {sent[key]={...entry};payload={...payload,[key]:entry.value}}
        }
        this.status='saving';this.publish()
        try {this.acknowledge(await this.api.save(payload),sent)}
        catch(error) {
          if(!this.api.isConflict(error))throw error
          // A logout can unmount this workspace while the PUT is in flight.
          // Do not issue a follow-up GET using a later account's session.
          if(this.stopped)return
          this.reconcile(await this.api.load())
        }
      }
      this.status=this.conflicts.length?'conflict':'idle'
    } catch(error) {
      this.status='error'
      this.error=error instanceof Error?error.message:undefined
    } finally {
      this.running=false
      this.persist();this.publish()
    }
  }
}
