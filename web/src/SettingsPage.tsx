import { useEffect, useState, type FormEvent } from 'react'
import { Check, FolderOpen, LoaderCircle } from 'lucide-react'
import { api, type DiscoveredModel, type ModelAttributes, type ModelProtocol, type ModelSettings, type Settings } from './api'

import {ModelRolesSettings} from './ModelRolesSettings'
import {SkillDirectorySettings} from './SkillDirectorySettings'

const message = (error: unknown) => error instanceof Error ? error.message : '请求未完成，请重试'

export function SettingsPage({ onSaved,openConnections,openProposals }: { onSaved: (value: Settings) => void; openConnections:()=>void; openProposals:()=>void }) {
  const [settings, setSettings] = useState<Settings>()
  const [error, setError] = useState('')

  useEffect(() => {
    let active = true
    api<Settings>('/settings').then(value => {
      if (active) {
        setSettings(value)
        onSaved(value)
      }
    }).catch(error => { if (active) setError(message(error)) })
    return () => { active = false }
  }, [onSaved])

  return <main className="main settings-page">
    <header className="page-heading"><h1>设置</h1></header>
    <nav aria-label="设置分区" className="model-settings-nav">
      <a href="#model-default">默认模型</a><a href="#model-profiles">模型档案</a><a href="#model-roles">角色绑定</a><a href="#skill-directory">Skill 目录</a>
    </nav>
    {settings ? <ModelForm initial={settings} onSaved={onSaved}/> : error ?
      <p className="error" role="alert">{error}</p> :
      <p className="muted" role="status">正在加载设置…</p>}
    <ModelRolesSettings/>
    <SkillDirectorySettings openProposals={openProposals}/>
    <section className="settings-card" aria-labelledby="settings-connections-title">
      <h2 id="settings-connections-title">MCP 工具与连接</h2>
      <p className="muted">管理连接、试连和工具授权。保存连接不试连；会话选择在对话内单独设置。</p>
      <button type="button" className="outline" onClick={openConnections}>打开工具与连接</button>
    </section>
  </main>
}

function fields(settings: Settings): ModelSettings {
  return { protocol: settings.protocol, base_url: settings.base_url, model_id: settings.model_id, api_key_ref: settings.api_key_ref, revision: settings.revision, attributes: settings.attributes ?? {} }
}

function ModelForm({ initial, onSaved }: { initial: Settings; onSaved: (value: Settings) => void }) {
  const [stored, setStored] = useState(initial)
  const [draft, setDraft] = useState(() => fields(initial))
  const [apiKey, setAPIKey] = useState('')
  const [removeKey, setRemoveKey] = useState(false)
  const [busy, setBusy] = useState<'save' | 'test' | 'discover'>()
  const [models, setModels] = useState<DiscoveredModel[]>()
  const [discoveryError, setDiscoveryError] = useState('')
  const [levels, setLevels] = useState(initial.attributes?.reasoning_levels?.join(', ') ?? '')
  const [tested, setTested] = useState(false)
  const [testError, setTestError] = useState('')
  const [saved, setSaved] = useState(false)
  const [saveError, setSaveError] = useState('')
  const keyChange = removeKey ? '' : apiKey.trim() || undefined
  const reasoningLevels = [...new Set(levels.split(/[,，]/).map(level => level.trim()).filter(Boolean))]
  const effort = draft.attributes?.reasoning_effort
  const attributes = { ...draft.attributes, reasoning_levels: reasoningLevels,
    reasoning_effort: effort && reasoningLevels.includes(effort) ? effort : undefined }
  const payload = { ...draft, attributes, api_key: keyChange }
  const attributesChanged = attributes.context_tokens !== stored.attributes?.context_tokens ||
    attributes.max_output_tokens !== stored.attributes?.max_output_tokens ||
    attributes.vision !== stored.attributes?.vision || attributes.reasoning !== stored.attributes?.reasoning || attributes.reasoning_effort !== stored.attributes?.reasoning_effort ||
    JSON.stringify(attributes.reasoning_levels) !== JSON.stringify(stored.attributes?.reasoning_levels ?? [])
  const dirty = attributesChanged || keyChange !== undefined || draft.protocol !== stored.protocol || draft.base_url !== stored.base_url || draft.model_id !== stored.model_id || draft.api_key_ref !== stored.api_key_ref

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
    setAPIKey('')
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
      const value = await api<Settings>('/settings', payload, 'PATCH')
      setStored(value)
      setDraft(fields(value))
      setLevels(value.attributes?.reasoning_levels?.join(', ') ?? '')
      setAPIKey('')
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
      await api<{duration_ms: number}>('/settings/model/test', payload)
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
      const result = await api<DiscoveredModel[]>('/settings/model/discover', payload)
      setModels(result)
      const selected = result.find(model => model.id === draft.model_id)
      if (selected) {
        const current = draft.attributes ?? {}
        setDraft(previous => ({...previous, attributes: {
          ...current,
          context_tokens: current.context_tokens ?? selected.attributes.context_tokens,
          max_output_tokens: current.max_output_tokens ?? selected.attributes.max_output_tokens,
          vision: current.vision ?? selected.attributes.vision,
          reasoning: current.reasoning ?? selected.attributes.reasoning,
        }}))
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

  function editAttribute<K extends keyof ModelAttributes>(key: K, value: ModelAttributes[K]) {
    setDraft(previous => ({...previous, attributes: {...previous.attributes, [key]: value}}))
    resetFeedback()
  }

  return <form id="model-default" className="settings-card" onSubmit={save}>
    <section className="settings-fields" aria-labelledby="model-settings-title">
      <header className="settings-section-heading">
        <h2 id="model-settings-title">默认模型</h2>
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
          <input type="password" value={apiKey} disabled={Boolean(busy)} maxLength={4096}
            placeholder={removeKey ? '保存后移除' : stored.local_key_saved ? '已保存，输入新密钥可替换' : '输入 API Key'}
            autoComplete="new-password" spellCheck={false}
            onChange={event => {setAPIKey(event.target.value); setRemoveKey(false); setModels(undefined); resetFeedback()}}/>
        </label>
        {stored.local_key_saved && <button type="button" className="outline" disabled={Boolean(busy)}
          onClick={() => {setAPIKey(''); setRemoveKey(!removeKey); setModels(undefined); resetFeedback()}}>
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
          keyChange ? '。测试使用新密钥，保存后生效。' : ''}
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
          <span>上下文窗口（Token）</span>
          <input type="number" min={1} step={1} placeholder="未知" value={attributes.context_tokens ?? ''}
            disabled={Boolean(busy)} onChange={event => editAttribute('context_tokens', event.target.value ? event.target.valueAsNumber : undefined)}/>
        </label>
        <label className="settings-field">
          <span>最大输出长度（Token）</span>
          <input type="number" min={1} step={1} placeholder="未知" value={attributes.max_output_tokens ?? ''}
            disabled={Boolean(busy)} onChange={event => editAttribute('max_output_tokens', event.target.value ? event.target.valueAsNumber : undefined)}/>
        </label>
        {([['vision', '图片输入'], ['reasoning', '可调推理强度']] as const).map(([key, label]) => <label className="settings-field" key={key}>
          <span>{label}</span>
          <select value={attributes[key] === undefined ? '' : String(attributes[key])} disabled={Boolean(busy)}
            onChange={event => {
              const value = event.target.value
              editAttribute(key, value ? value === 'true' : undefined)
              if (key === 'reasoning' && value === 'false') {
                setLevels('')
                editAttribute('reasoning_effort', undefined)
              }
            }}>
            <option value="">未知</option><option value="true">支持</option><option value="false">不支持</option>
          </select>
        </label>)}
        <label className="settings-field">
          <span>推理强度档位</span>
          <input placeholder="例如 low, medium, high" value={levels} maxLength={528}
            disabled={Boolean(busy) || attributes.reasoning === false}
            onChange={event => {setLevels(event.target.value); resetFeedback()}}/>

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
