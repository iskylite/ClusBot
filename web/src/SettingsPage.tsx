import { useEffect, useState, type FormEvent } from 'react'
import { Check, FolderOpen, LoaderCircle } from 'lucide-react'
import { api, type ModelSettings, type Settings } from './api'

const message = (error: unknown) => error instanceof Error ? error.message : '请求未完成，请重试'

export function SettingsPage({ onSaved }: { onSaved: (value: Settings) => void }) {
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
    {settings ? <ModelForm initial={settings} onSaved={onSaved}/> : error ?
      <p className="error" role="alert">{error}</p> :
      <p className="muted" role="status">正在加载设置…</p>}
  </main>
}

function fields(settings: Settings): ModelSettings {
  return { base_url: settings.base_url, model_id: settings.model_id, api_key_ref: settings.api_key_ref, revision: settings.revision }
}

function ModelForm({ initial, onSaved }: { initial: Settings; onSaved: (value: Settings) => void }) {
  const [stored, setStored] = useState(initial)
  const [draft, setDraft] = useState(() => fields(initial))
  const [busy, setBusy] = useState<'save' | 'test'>()
  const [tested, setTested] = useState(false)
  const [testError, setTestError] = useState('')
  const [saved, setSaved] = useState(false)
  const [saveError, setSaveError] = useState('')
  const dirty = draft.base_url !== stored.base_url || draft.model_id !== stored.model_id || draft.api_key_ref !== stored.api_key_ref

  function resetFeedback() {
    setTested(false)
    setTestError('')
    setSaved(false)
    setSaveError('')
  }

  function edit(key: 'base_url' | 'model_id' | 'api_key_ref', value: string) {
    setDraft(previous => ({...previous, [key]: value}))
    resetFeedback()
  }

  async function save(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (busy || !dirty) return
    setBusy('save')
    setSaveError('')
    try {
      const value = await api<Settings>('/settings', draft, 'PATCH')
      setStored(value)
      setDraft(fields(value))
      onSaved(value)
      setSaved(true)
    } catch (error) {
      setSaveError(message(error))
    } finally {
      setBusy(undefined)
    }
  }

  async function test() {
    if (busy) return
    setBusy('test')
    setTested(false)
    setTestError('')
    try {
      await api<{duration_ms: number}>('/settings/model/test', draft)
      setTested(true)
    } catch (error) {
      setTestError(message(error))
    } finally {
      setBusy(undefined)
    }
  }

  return <form className="settings-card" onSubmit={save}>
    <section className="settings-fields" aria-labelledby="model-settings-title">
      <header className="settings-section-heading">
        <h2 id="model-settings-title">模型</h2>
        {stored.mode === 'demo' && <span className="mode-note">当前使用本地演示</span>}
      </header>
      <div className="settings-model-row">
        <label className="settings-field">
          <span>协议</span>
          <input value="Responses" readOnly/>
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
          <span>密钥环境变量</span>
          <input value={draft.api_key_ref.slice(4)} disabled={Boolean(busy)} maxLength={80} required
            pattern="[A-Za-z_][A-Za-z0-9_]*" autoComplete="off" spellCheck={false}
            onChange={event => edit('api_key_ref', 'env:' + event.target.value)}/>
        </label>
        {draft.api_key_ref === stored.api_key_ref && <span className={stored.key_configured ? 'memory-saved' : 'connection-status'}>
          {stored.key_configured && <Check size={15}/>}
          {stored.key_configured ? '已读取密钥' : '未读取密钥'}
        </span>}
      </div>
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
        onClick={() => { setDraft(fields(stored)); resetFeedback() }}>取消更改</button>
      <button type="submit" className="primary" disabled={!dirty || Boolean(busy)}>{busy === 'save' ? '正在保存…' : '保存设置'}</button>
    </div>
  </form>
}
