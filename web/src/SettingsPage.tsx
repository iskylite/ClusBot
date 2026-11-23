import { useCallback, useEffect, useRef, useState, type FormEvent } from 'react'
import { Check, FolderOpen, LoaderCircle } from 'lucide-react'
import { api, type DiscoveredModel, type ModelAttributes, type ModelProtocol, type ModelSettings, type Settings } from './api'

import {ModelRolesSettings} from './ModelRolesSettings'
import {SkillDirectorySettings} from './SkillDirectorySettings'
import {ModelSettings as ModelCatalogSettings} from './ModelSettings'

const message = (error: unknown) => error instanceof Error ? error.message : '请求未完成，请重试'

export function SettingsPage({ onSaved,openProposals,view='model' }: { onSaved: (value: Settings) => void; openConnections:()=>void; openProposals:()=>void; view?:'model'|'skills' }) {
  const [settings, setSettings] = useState<Settings & {default_source?:string}>()
  const [error, setError] = useState('')
  const [modelRevision, setModelRevision] = useState(0)
  const [anchor, setAnchor] = useState(location.hash)

  const refreshSettings = useCallback(async () => {
    const value = await api<Settings & {default_source?:string}>('/settings')
    setSettings(value)
    setModelRevision(revision => revision + 1)
    onSaved(value)
  }, [onSaved])

  useEffect(() => {
    if(view!=='model') return
    let active = true
    api<Settings & {default_source?:string}>('/settings').then(value => {
      if (active) {
        setSettings(value)
        onSaved(value)
      }
    }).catch(error => { if (active) setError(message(error)) })
    return () => { active = false }
  }, [onSaved,view])

  useEffect(()=>{
    const update=()=>setAnchor(location.hash)
    window.addEventListener('hashchange',update)
    window.addEventListener('popstate',update)
    window.addEventListener('clusbot:navigate',update)
    return()=>{window.removeEventListener('hashchange',update);window.removeEventListener('popstate',update);window.removeEventListener('clusbot:navigate',update)}
  },[])
  useEffect(()=>{
    if(anchor!=='#model-roles')return
    const frame=requestAnimationFrame(()=>document.getElementById(anchor.slice(1))?.scrollIntoView({block:'start'}))
    return()=>cancelAnimationFrame(frame)
  },[anchor,Boolean(settings)])

  const legacyModelDetail=['#model-default','#model-profiles','#model-roles'].includes(anchor)
  const modelRoutingView=anchor==='#model-routing'
  function navigateModel(target:string){
    if(location.pathname+location.search+location.hash!==target){
      history.pushState(null,'',target)
      window.dispatchEvent(new Event('clusbot:navigate'))
    }
  }

  return <main className={'main settings-page'+(view==='skills'?' settings-skills-page':'')}>
    <h1 className="settings-sr-only">{view==='skills'?'Skills':'模型'}</h1>
    {view==='skills' ? <SkillDirectorySettings openProposals={openProposals}/> : <>
      {legacyModelDetail ? <a className="model-link" href="/settings?tab=model">返回模型设置</a> : (
        <nav className="model-subnav" aria-label="模型设置分区">
          <button type="button" aria-current={!modelRoutingView?'page':undefined} onClick={()=>navigateModel('/settings?tab=model')}>提供方与模型</button>
          <button type="button" aria-current={modelRoutingView?'page':undefined} onClick={()=>navigateModel('/settings?tab=model#model-routing')}>阶段模型策略</button>
        </nav>
      )}
      {anchor==='#model-default' ? settings?.default_source === 'profile' ?
        <LegacyDefault onRestored={refreshSettings}/> : settings ? <ModelForm key={settings.revision} initial={settings} onSaved={onSaved}/> : error ?
          <p className="error" role="alert">{error}</p> : <p className="muted" role="status">正在加载设置…</p> :
        anchor==='#model-profiles'||anchor==='#model-roles' ? <ModelRolesSettings refreshKey={modelRevision}/> :
        <ModelCatalogSettings onChanged={refreshSettings} mode={modelRoutingView?'routing':'catalog'}/>}
    </>}
  </main>
}

function LegacyDefault({onRestored}:{onRestored:()=>Promise<void>}) {
  const [busy,setBusy]=useState(false)
  const [error,setError]=useState('')
  async function restore() {
    setBusy(true);setError('')
    try {
      const current=await api<{revision:number}>('/settings/model/default')
      await api('/settings/model/default',{source:'legacy_default',revision:current.revision},'PUT')
      await onRestored()
    } catch(error) { setError(message(error)) } finally { setBusy(false) }
  }
  return <section id="model-default" className="settings-card"><h2>旧默认模型</h2>
    <p className="muted">当前默认模型来自目录。恢复旧默认模型后可编辑其设置。</p>
    <div><button type="button" className="outline" disabled={busy} onClick={()=>void restore()}>{busy?'正在恢复…':'恢复旧默认模型'}</button></div>
    {error&&<p className="error" role="alert">{error}</p>}
  </section>
}

function fields(settings: Settings): ModelSettings {
  return { protocol: settings.protocol, base_url: settings.base_url, model_id: settings.model_id, api_key_ref: settings.api_key_ref, revision: settings.revision, attributes: settings.attributes ?? {} }
}

function ModelForm({ initial, onSaved }: { initial: Settings; onSaved: (value: Settings) => void }) {
  const [stored, setStored] = useState(initial)
  const [draft, setDraft] = useState(() => fields(initial))
  const apiKeyInput = useRef<HTMLInputElement>(null)
  const [hasAPIKey, setHasAPIKey] = useState(false)
  const [removeKey, setRemoveKey] = useState(false)
  const [busy, setBusy] = useState<'save' | 'test' | 'discover'>()
  const [models, setModels] = useState<DiscoveredModel[]>()
  const [discoveryError, setDiscoveryError] = useState('')
  const [levels, setLevels] = useState(initial.attributes?.reasoning_levels?.join(', ') ?? '')
  const [tested, setTested] = useState(false)
  const [testError, setTestError] = useState('')
  const [saved, setSaved] = useState(false)
  const [saveError, setSaveError] = useState('')
  const reasoningLevels = [...new Set(levels.split(/[,，]/).map(level => level.trim()).filter(Boolean))]
  const effort = draft.attributes?.reasoning_effort
  const attributes = { ...draft.attributes, reasoning_levels: reasoningLevels,
    reasoning_effort: effort && reasoningLevels.includes(effort) ? effort : undefined }
  const payload = () => ({ ...draft, attributes, api_key: removeKey ? '' : apiKeyInput.current?.value.trim() || undefined })
  const attributesChanged = attributes.context_tokens !== stored.attributes?.context_tokens ||
    attributes.max_output_tokens !== stored.attributes?.max_output_tokens ||
    attributes.tool_calling !== stored.attributes?.tool_calling || attributes.structured_output !== stored.attributes?.structured_output || attributes.file_input !== stored.attributes?.file_input ||
    attributes.vision !== stored.attributes?.vision || attributes.reasoning !== stored.attributes?.reasoning || attributes.reasoning_effort !== stored.attributes?.reasoning_effort ||
    JSON.stringify(attributes.reasoning_levels) !== JSON.stringify(stored.attributes?.reasoning_levels ?? []) ||
    JSON.stringify(attributes.sources ?? {}) !== JSON.stringify(stored.attributes?.sources ?? {})
  const dirty = attributesChanged || removeKey || hasAPIKey || draft.protocol !== stored.protocol || draft.base_url !== stored.base_url || draft.model_id !== stored.model_id || draft.api_key_ref !== stored.api_key_ref

  function resetFeedback() {
    setTested(false)
    setTestError('')
    setSaved(false)
    setSaveError('')
    setDiscoveryError('')
  }

  function cancelChanges() {
    setDraft(fields(stored))
    setLevels(stored.attributes?.reasoning_levels?.join(', ') ?? '')
    setModels(undefined)
    if (apiKeyInput.current) apiKeyInput.current.value = ''
    setHasAPIKey(false)
    setRemoveKey(false)
    resetFeedback()
  }

  function edit<K extends keyof ModelSettings>(key: K, value: ModelSettings[K]) {
    const modelChanged = key === 'model_id' || key === 'base_url' || key === 'protocol'
    if (modelChanged) setLevels('')
    if (key !== 'model_id') setModels(undefined)
    setDraft(previous => ({...previous, [key]: value, attributes: modelChanged ? {} : previous.attributes}))
    resetFeedback()
  }

  async function save(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (busy || !dirty) return
    setBusy('save')
    setSaveError('')
    try {
      const value = await api<Settings>('/settings', payload(), 'PATCH')
      setStored(value)
      setDraft(fields(value))
      setLevels(value.attributes?.reasoning_levels?.join(', ') ?? '')
      if (apiKeyInput.current) apiKeyInput.current.value = ''
      setHasAPIKey(false)
      setRemoveKey(false)
      setTested(false)
      setTestError('')
      onSaved(value)
      setSaved(true)
    } catch (error) {
      setSaveError(message(error))
    } finally {
      setBusy(undefined)
    }
  }

  async function test() {
    if (busy || !draft.model_id.trim()) return
    setBusy('test')
    setTested(false)
    setTestError('')
    try {
      await api<{duration_ms: number}>('/settings/model/test', payload())
      setTested(true)
    } catch (error) {
      setTestError(message(error))
    } finally {
      setBusy(undefined)
    }
  }

  async function discover() {
    if (busy) return
    setBusy('discover')
    setDiscoveryError('')
    setModels(undefined)
    try {
      const result = await api<DiscoveredModel[]>('/settings/model/discover', payload())
      setModels(result)
      const selected = result.find(model => model.id === draft.model_id)
      if (selected) {
        const current = draft.attributes ?? {}
        const merged:ModelAttributes = {...current, sources:{...current.sources}}
        for (const key of ['context_tokens','max_output_tokens','vision','file_input','tool_calling','structured_output','reasoning'] as const) {
          if (current[key] === undefined && current.sources?.[key] !== 'manual' && selected.attributes[key] !== undefined) {
            Object.assign(merged, {[key]:selected.attributes[key]})
            const origin = selected.attributes.sources?.[key]
            if (origin) merged.sources![key] = origin
          }
        }
        if (!reasoningLevels.length && current.sources?.reasoning_levels !== 'manual' && merged.reasoning !== false && selected.attributes.reasoning_levels?.length) {
          merged.reasoning_levels = selected.attributes.reasoning_levels
          setLevels(selected.attributes.reasoning_levels.join(', '))
          const origin = selected.attributes.sources?.reasoning_levels
          if (origin) merged.sources!.reasoning_levels = origin
        }
        setDraft(previous => ({...previous, attributes:merged}))
      }
    } catch (error) {
      setDiscoveryError(message(error))
    } finally {
      setBusy(undefined)
    }
  }

  function chooseModel(id: string) {
    const selected = models?.find(model => model.id === id)
    if (!selected || id === draft.model_id) return
    setDraft(previous => ({...previous, model_id: id, attributes: selected.attributes}))
    setLevels(selected.attributes.reasoning_levels?.join(', ') ?? '')
    resetFeedback()
  }

  function editAttribute<K extends Exclude<keyof ModelAttributes,'sources'>>(key: K, value: ModelAttributes[K]) {
    setDraft(previous => ({...previous, attributes: {...previous.attributes, [key]: value, sources:{...previous.attributes?.sources,[key]:'manual'}}}))
    resetFeedback()
  }
  function editLevels(value:string) {
    setLevels(value)
    setDraft(previous => ({...previous, attributes:{...previous.attributes,sources:{...previous.attributes?.sources,reasoning_levels:'manual'}}}))
    resetFeedback()
  }
  function attributeSource(key:Exclude<keyof ModelAttributes,'sources'>) {
    const origin = attributes.sources?.[key]
    const value = key === 'reasoning_levels' ? reasoningLevels : attributes[key]
    return origin === 'provider' ? '服务声明' : origin === 'manual' ? '手动' : value !== undefined && (!Array.isArray(value) || value.length > 0) ? '来源未记录' : '未知'
  }

  return <form id="model-default" className="settings-card" onSubmit={save}>
    <section className="settings-fields" aria-labelledby="model-settings-title">
      <header className="settings-section-heading">
        <h2 id="model-settings-title">旧默认模型</h2>
        {stored.mode === 'demo' && <span className="mode-note">当前使用本地演示</span>}
      </header>
      <div className="settings-model-row">
        <label className="settings-field">
          <span>协议</span>
          <select value={draft.protocol} disabled={Boolean(busy)}
            onChange={event => edit('protocol', event.target.value as ModelProtocol)}>
            <option value="chat_completions">Chat Completions</option>
            <option value="responses">Responses</option>
          </select>
        </label>
        <label className="settings-field">
          <span>模型 ID</span>
          <input value={draft.model_id} readOnly={Boolean(stored.model_id_source)} disabled={Boolean(busy)} maxLength={256}
            placeholder="填写服务提供的模型 ID" onChange={event => edit('model_id', event.target.value)}
            aria-describedby={stored.model_id_source ? 'model-id-source' : undefined}/>
          {stored.model_id_source && <small id="model-id-source">由 {stored.model_id_source} 覆盖</small>}
        </label>
      </div>
      <label className="settings-field">
        <span>服务地址</span>
        <input type="url" value={draft.base_url} readOnly={Boolean(stored.base_url_source)} disabled={Boolean(busy)}
          maxLength={2048} required onChange={event => edit('base_url', event.target.value)}
          aria-describedby={stored.base_url_source ? 'base-url-source' : undefined}/>
        {stored.base_url_source && <small id="base-url-source">由 {stored.base_url_source} 覆盖</small>}
      </label>
      <div className="settings-key">
        <label className="settings-field">
          <span>API Key</span>
          <input ref={apiKeyInput} type="password" disabled={Boolean(busy)} maxLength={4096}
            placeholder={removeKey ? '保存后移除' : stored.local_key_saved ? '已保存，输入新密钥可替换' : '输入 API Key'}
            autoComplete="new-password" spellCheck={false}
            onChange={event => {setHasAPIKey(Boolean(event.currentTarget.value.trim())); setRemoveKey(false); setModels(undefined); resetFeedback()}}/>
        </label>
        {stored.local_key_saved && <button type="button" className="outline" disabled={Boolean(busy)}
          onClick={() => {if (apiKeyInput.current) apiKeyInput.current.value = ''; setHasAPIKey(false); setRemoveKey(!removeKey); setModels(undefined); resetFeedback()}}>
          {removeKey ? '取消移除' : '移除密钥'}
        </button>}
      </div>
      <div className="settings-key">
        <label className="settings-field">
          <span>备用密钥环境变量</span>
          <input value={draft.api_key_ref.slice(4)} disabled={Boolean(busy)} maxLength={80} required
            pattern="[A-Za-z_][A-Za-z0-9_]*" autoComplete="off" spellCheck={false}
            onChange={event => edit('api_key_ref', 'env:' + event.target.value)}/>
        </label>

      </div>
      <p className="muted" role="status">
        {stored.local_key_saved ? '当前使用本地保存的密钥' :
          stored.key_configured ? '当前使用环境变量 ' + stored.api_key_ref.slice(4) : '当前未配置密钥'}
        {removeKey ? '。测试和保存后将使用备用环境变量。' :
          hasAPIKey ? '。测试使用新密钥，保存后生效。' : ''}
      </p>
      <div className="settings-actions">
        {discoveryError && <p className="error" role="alert">{discoveryError}，可以手动填写模型 ID。</p>}
        {models?.length === 0 && <p className="muted" role="status">服务未返回可选模型。</p>}
        <button type="button" className="outline" disabled={Boolean(busy) || !draft.base_url.trim()} onClick={discover}>
          {busy === 'discover' ? '正在获取…' : '获取模型'}
        </button>
      </div>
      {!!models?.length && !stored.model_id_source && <label className="settings-field">
        <span>服务返回的模型</span>
        <select value={models.some(model => model.id === draft.model_id) ? draft.model_id : ''} disabled={Boolean(busy)}
          onChange={event => chooseModel(event.target.value)}>
          <option value="">选择模型</option>
          {models.map(model => <option key={model.id} value={model.id}>{model.id}</option>)}
        </select>
      </label>}
      <section className="settings-fields settings-model-attributes" aria-labelledby="model-attributes-title">
        <h3 id="model-attributes-title">模型属性</h3>
        <label className="settings-field">
          <span>上下文窗口（Token） · {attributeSource('context_tokens')}</span>
          <input type="number" min={1} step={1} placeholder="未知" value={attributes.context_tokens ?? ''}
            disabled={Boolean(busy)} onChange={event => editAttribute('context_tokens', event.target.value ? event.target.valueAsNumber : undefined)}/>
        </label>
        <label className="settings-field">
          <span>最大输出长度（Token） · {attributeSource('max_output_tokens')}</span>
          <input type="number" min={1} step={1} placeholder="未知" value={attributes.max_output_tokens ?? ''}
            disabled={Boolean(busy)} onChange={event => editAttribute('max_output_tokens', event.target.value ? event.target.valueAsNumber : undefined)}/>
        </label>
        {([['vision', '图片输入'], ['reasoning', '可调推理强度']] as const).map(([key, label]) => <label className="settings-field" key={key}>
          <span>{label} · {attributeSource(key)}</span>
          <select value={attributes[key] === undefined ? '' : String(attributes[key])} disabled={Boolean(busy)}
            onChange={event => {
              const value = event.target.value
              editAttribute(key, value ? value === 'true' : undefined)
              if (key === 'reasoning' && value === 'false') {
                editLevels('')
                editAttribute('reasoning_effort', undefined)
              }
            }}>
            <option value="">未知</option><option value="true">支持</option><option value="false">不支持</option>
          </select>
        </label>)}
        <label className="settings-field">
          <span>推理强度档位 · {attributeSource('reasoning_levels')}</span>
          <input placeholder="例如 low, medium, high" value={levels} maxLength={528}
            disabled={Boolean(busy) || attributes.reasoning === false}
            onChange={event => editLevels(event.target.value)}/>

        </label>
        {!!reasoningLevels.length && attributes.reasoning !== false && <label className="settings-field">
          <span>使用的推理强度</span>
          <select value={attributes.reasoning_effort ?? ''} disabled={Boolean(busy)}
            onChange={event => editAttribute('reasoning_effort', event.target.value || undefined)}>
            <option value="">服务默认</option>
            {reasoningLevels.map(level => <option key={level} value={level}>{level}</option>)}
          </select>
        </label>}
      </section>
      <div className="settings-actions">
        <div>
          {tested && <span className="memory-saved" role="status"><Check size={15}/>文本流、工具调用与结果回传均通过</span>}
          {testError && <p className="error" role="alert">{testError}</p>}
        </div>
        <button type="button" className="outline" disabled={Boolean(busy) || !draft.model_id.trim()} onClick={test}>
          {busy === 'test' && <LoaderCircle className="spin" size={15}/>}
          {busy === 'test' ? '正在测试…' : '测试连接'}
        </button>
      </div>
    </section>
    <section className="settings-workspace" aria-labelledby="workspace-settings-title">
      <header className="settings-section-heading"><h2 id="workspace-settings-title">工作区</h2><small>启动配置 · 只读</small></header>
      <div className="settings-path"><FolderOpen size={17} aria-hidden="true"/><span>{stored.workspace_path}</span></div>
    </section>
    <div className="settings-actions settings-save">
      <div>
        {saved && <span className="memory-saved" role="status"><Check size={15}/>已保存</span>}
        {saveError && <p className="error" role="alert">{saveError}</p>}
      </div>
      <button type="button" className="quiet" disabled={!dirty || Boolean(busy)}
        onClick={cancelChanges}>取消更改</button>
      <button type="submit" className="primary" disabled={!dirty || Boolean(busy)}>{busy === 'save' ? '正在保存…' : '保存设置'}</button>
    </div>
  </form>
}
