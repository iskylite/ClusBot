import assert from 'node:assert/strict'
import {readFile} from 'node:fs/promises'
import test from 'node:test'
import ts from 'typescript'

const source=await readFile(new URL('../src/preferencesController.ts',import.meta.url),'utf8')
const js=ts.transpileModule(source,{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext}}).outputText
const {PreferencesController}=await import('data:text/javascript;base64,'+Buffer.from(js).toString('base64'))
const defaults=()=>({language:'system',timezone:'system',appearance:'system',reduce_motion:false,send_shortcut:'enter',desktop_notifications:false,revision:0})
const conflict=()=>Object.assign(new Error('revision conflict'),{status:409})
const wait=()=>new Promise(resolve=>setImmediate(resolve))
async function until(condition) {for(let i=0;i<100;i++){if(condition())return;await wait()}throw new Error('condition did not settle')}
function storage() {
  const values=new Map()
  return {getItem:key=>values.get(key)??null,setItem:(key,value)=>values.set(key,value),removeItem:key=>values.delete(key)}
}
function deferred(){let resolve,reject;const promise=new Promise((a,b)=>{resolve=a;reject=b});return {promise,resolve,reject}}
function server(initial=defaults()) {
  let value={...initial}
  let writes=0
  return {
    get value(){return {...value}}, get writes(){return writes},
    change(patch){value={...value,...patch,revision:value.revision+1}},
    api:{load:async()=>({...value}),save:async next=>{
      writes++
      if(next.revision!==value.revision)throw conflict()
      value={...next,revision:next.revision+1}
      return {...value}
    },isConflict:error=>error?.status===409},
  }
}
async function ready(controller) {controller.start();await until(()=>controller.snapshot().status==='idle')}

test('serializes rapid changes and preserves edits made during an in-flight write',async()=>{
  const remote=server(), gate=deferred()
  let first=true
  const api={...remote.api,save:async value=>{if(first){first=false;await gate.promise}return remote.api.save(value)}}
  const controller=new PreferencesController(api,storage(),'prefs')
  await ready(controller)
  controller.update('appearance','dark')
  controller.update('appearance','light')
  controller.update('language','en-US')
  assert.equal(controller.snapshot().value.appearance,'light')
  gate.resolve()
  await until(()=>controller.snapshot().status==='idle')
  assert.equal(remote.value.appearance,'light')
  assert.equal(remote.value.language,'en-US')
  assert.equal(remote.value.revision,2)
})

test('rebases independent changes and requires a decision for conflicting fields',async()=>{
  const remote=server(), controller=new PreferencesController(remote.api,storage(),'prefs')
  await ready(controller)
  remote.change({language:'en-US'})
  controller.update('appearance','dark')
  await until(()=>controller.snapshot().status==='idle')
  assert.equal(remote.value.language,'en-US')
  assert.equal(remote.value.appearance,'dark')
  remote.change({appearance:'light'})
  controller.update('appearance','system')
  await until(()=>controller.snapshot().status==='conflict')
  assert.deepEqual(controller.snapshot().conflicts,['appearance'])
  assert.equal(controller.snapshot().value.appearance,'system')
  assert.equal(remote.value.appearance,'light')
  controller.resolveConflicts(true)
  await until(()=>controller.snapshot().status==='idle')
  assert.equal(remote.value.appearance,'system')
  assert.equal(remote.value.language,'en-US')
})

test('retains a failed edit across reload and reconciles a lost response without another write',async()=>{
  const remote=server(), saved=storage()
  let lost=true
  const api={...remote.api,save:async value=>{
    const result=await remote.api.save(value)
    if(lost){lost=false;throw new TypeError('connection lost')}
    return result
  }}
  const first=new PreferencesController(api,saved,'prefs')
  await ready(first)
  first.update('timezone','Asia/Shanghai')
  await until(()=>first.snapshot().status==='error')
  assert.equal(first.snapshot().value.timezone,'Asia/Shanghai')
  assert.notEqual(saved.getItem('prefs'),null)
  first.stop()
  const restored=new PreferencesController(api,saved,'prefs')
  await ready(restored)
  assert.equal(restored.snapshot().value.timezone,'Asia/Shanghai')
  assert.equal(remote.writes,1)
  assert.equal(saved.getItem('prefs'),null)
})

test('stopping a workspace leaves later edits queued instead of sending under a new session',async()=>{
  const remote=server(), gate=deferred(), saved=storage()
  const api={...remote.api,save:async value=>{await gate.promise;return remote.api.save(value)}}
  const controller=new PreferencesController(api,saved,'prefs')
  await ready(controller)
  controller.update('appearance','dark')
  controller.update('language','en-US')
  controller.stop()
  gate.resolve()
  await until(()=>remote.writes===1)
  await wait()
  assert.equal(remote.value.appearance,'dark')
  assert.equal(remote.value.language,'system')
  assert.notEqual(saved.getItem('prefs'),null)
  const restored=new PreferencesController(remote.api,saved,'prefs')
  await ready(restored)
  assert.equal(remote.value.language,'en-US')
})

test('does not reload after a stale write returns once the workspace has stopped',async()=>{
  const remote=server(), gate=deferred(), saved=storage()
  let loads=0
  const api={...remote.api,load:async()=>{loads++;return remote.api.load()},save:async value=>{await gate.promise;return remote.api.save(value)}}
  const controller=new PreferencesController(api,saved,'prefs')
  await ready(controller)
  controller.update('appearance','dark')
  remote.change({language:'en-US'})
  controller.stop()
  gate.resolve()
  await until(()=>remote.writes===1)
  await wait()
  assert.equal(loads,1)
  assert.notEqual(saved.getItem('prefs'),null)
})
