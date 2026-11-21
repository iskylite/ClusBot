import {useEffect, useState, type FormEvent} from 'react'
import {api, APIError, type Overview} from './api'

export type Preferences = {
  language: 'system'|'zh-CN'|'en-US'
  timezone: string
  appearance: 'system'|'light'|'dark'
  reduce_motion: boolean
  send_shortcut: 'enter'|'mod_enter'
  desktop_notifications: boolean
  revision: number
}

const label = (zh: string, en: string, english: boolean) => english ? en : zh
const defaults: Preferences = {
  language: 'system', timezone: 'system', appearance: 'system',
  reduce_motion: false, send_shortcut: 'mod_enter', desktop_notifications: false, revision: 0,
}
const message = (error: unknown) => error instanceof Error ? error.message : '请求未完成'

export function uiLanguage(language: Preferences['language'] = 'system') {
  const locale = navigator.language
  return language === 'system' ? (locale.startsWith('en') ? 'en-US' : 'zh-CN') : language
}

export function displayDate(value: number, options: Intl.DateTimeFormatOptions) {
  const root = document.documentElement
  return new Intl.DateTimeFormat(root.lang || 'zh-CN', {
    ...options, timeZone: options.timeZone ?? root.dataset.timezone,
  }).format(value)
}

export function applyPreferences(preferences: Preferences) {
  const root = document.documentElement
  root.lang = uiLanguage(preferences.language)
  root.dataset.appearance = preferences.appearance
  root.dataset.reduceMotion = String(preferences.reduce_motion)
  root.dataset.sendShortcut = preferences.send_shortcut
  if (preferences.timezone === 'system') delete root.dataset.timezone
  else root.dataset.timezone = preferences.timezone
}

type NoticeKind = 'completed'|'waiting_user'|'interrupted'|'test'

const notificationBody = {
  completed: ['有任务已完成。', 'A task has completed.'],
  waiting_user: ['有任务需要你确认。', 'A task needs your attention.'],
  interrupted: ['有任务已中断。', 'A task was interrupted.'],
  test: ['系统通知测试。', 'System notification test.'],
}

function hostNotifications() {
  return window.webkit?.messageHandlers?.clusbotNotifications
}

async function notificationPermission(request = false) {
  const native = hostNotifications()
  if (native) return native.postMessage({action: request ? 'request' : 'status'})
  if (!('Notification' in window)) return 'unsupported'
  return request && Notification.permission === 'default' ? Notification.requestPermission() : Notification.permission
}

async function notify(kind: NoticeKind, id: string) {
  const native = hostNotifications()
  if (native) return (await native.postMessage({action:'send',kind,id})) === 'sent'
  if (typeof Notification === 'undefined' || Notification.permission !== 'granted') return false
  const english = document.documentElement.lang === 'en-US'
  new Notification('ClusBot', {body: notificationBody[kind][english ? 1 : 0], tag: id})
  return true
}

export function useTaskNotifications(enabled: boolean) {
  useEffect(() => {
    if (!enabled) return
    let active = true
    let loading = false
    let previousSnapshot: number | undefined
    const delivered = new Set<string>()
    async function poll() {
      if (loading) return
      loading = true
      try {
        const overview = await api<Overview>('/overview?limit=8')
        if (!active) return
        const tasks = [...overview.groups.waiting, ...overview.groups.active, ...overview.groups.completed]
        for (const task of tasks) {
          const kind = task.status
          if (kind !== 'completed' && kind !== 'waiting_user' && kind !== 'interrupted') continue
          const key = task.id + ':' + kind + ':' + task.state_changed_at
          if (previousSnapshot !== undefined && task.state_changed_at >= previousSnapshot && !delivered.has(key)) {
            if (!active) return
            if (await notify(kind, key)) delivered.add(key)
          }
        }
        previousSnapshot = overview.snapshot_at
        if (delivered.size > 256) {
          const retained = [...delivered].slice(-128)
          delivered.clear()
          for (const key of retained) delivered.add(key)
        }
      } catch {
        // The next successful poll can still observe recent transitions.
      } finally { loading = false }
    }
    void poll()
    const timer = window.setInterval(() => void poll(), 5000)
    return () => { active = false; window.clearInterval(timer) }
  }, [enabled])
}

export function GeneralPreferencesPage({value,onSaved,multiuser}: {
  value?:Preferences; onSaved:(value:Preferences)=>void; multiuser:boolean
}) {
  const [draft,setDraft] = useState(value ?? defaults)
  const [busy,setBusy] = useState(false)
  const [error,setError] = useState('')
  const [notice,setNotice] = useState('')
  const [permission,setPermission] = useState('')
  const english = uiLanguage(value?.language) === 'en-US'
  const t = (zh:string,en:string) => label(zh,en,english)

  useEffect(() => {if (value) setDraft(value)}, [value])
  useEffect(() => {void notificationPermission().then(setPermission).catch(() => setPermission('unsupported'))}, [])
  function change<K extends keyof Preferences>(key:K, next:Preferences[K]) {
    setDraft(previous => ({...previous,[key]:next}))
    setError('')
    setNotice('')
  }
  async function save(event:FormEvent) {
    event.preventDefault()
    if (!value || busy) return
    setBusy(true)
    setError('')
    setNotice('')
    try {
      const result = await api<Preferences>('/preferences',draft,'PUT')
      onSaved(result)
      setNotice(t('设置已保存','Preferences saved'))
    } catch (error) {
      setError(message(error))
      if (error instanceof APIError && error.status === 409) {
        try {
          const latest = await api<Preferences>('/preferences')
          setDraft(previous => ({...previous,revision:latest.revision}))
          setError(t('其他会话更新了设置。已保留当前输入，核对后可再次保存。','Preferences changed in another session. Review your edits and save again.'))
        } catch (refreshError) {setError(message(refreshError))}
      }
    } finally {setBusy(false)}
  }
  async function testNotification() {
    setError('')
    setNotice('')
    try {
      const status = await notificationPermission(true)
      setPermission(status)
      if (status === 'granted' && await notify('test',crypto.randomUUID())) {
        setNotice(t('系统已接收测试通知','Test notification submitted to the system'))
      } else {
        setError(t('未能投递测试通知，请检查系统或浏览器的通知权限。','Check this app’s notification permissions and try again.'))
      }
    } catch (error) {setError(message(error))}
  }

  const systemZone = Intl.DateTimeFormat().resolvedOptions().timeZone
  return <form className="preferences-form" onSubmit={save}>
    <section className="settings-section">
      <h2>{t('常规偏好','General preferences')}</h2>
      <div className="preference-row"><label htmlFor="pref-language">{t('界面语言','Interface language')}</label><select id="pref-language" value={draft.language} onChange={event=>change('language',event.currentTarget.value as Preferences['language'])}>
        <option value="system">{t('跟随系统','System')}</option><option value="zh-CN">简体中文</option><option value="en-US">English</option>
      </select></div>
      <div className="preference-row"><label htmlFor="pref-timezone">{t('显示时区','Display time zone')}</label><input id="pref-timezone" list="preference-timezones" value={draft.timezone} required maxLength={128} onChange={event=>change('timezone',event.currentTarget.value)}/>
        <datalist id="preference-timezones"><option value="system" label={systemZone}/>{Array.from(new Set(['Asia/Shanghai','UTC',systemZone,...(Intl.supportedValuesOf?.('timeZone') ?? [])])).filter(Boolean).map(zone=><option key={zone} value={zone}/>)}</datalist>
      </div>
      <div className="preference-row"><label htmlFor="pref-appearance">{t('外观','Appearance')}</label><select id="pref-appearance" value={draft.appearance} onChange={event=>change('appearance',event.currentTarget.value as Preferences['appearance'])}>
        <option value="system">{t('跟随系统','System')}</option><option value="light">{t('浅色','Light')}</option><option value="dark">{t('深色','Dark')}</option>
      </select></div>
      <label className="preference-row"><span>{t('减弱动态效果','Reduce motion')}</span><input type="checkbox" checked={draft.reduce_motion} onChange={event=>change('reduce_motion',event.currentTarget.checked)}/></label>
      <div className="preference-row"><label htmlFor="pref-send-shortcut">{t('发送快捷键','Send shortcut')}</label><select id="pref-send-shortcut" value={draft.send_shortcut} onChange={event=>change('send_shortcut',event.currentTarget.value as Preferences['send_shortcut'])}>
        <option value="mod_enter">Ctrl / ⌘ + Enter</option><option value="enter">Enter</option>
      </select></div>
    </section>
    <section className="settings-section">
      <h2>{t('桌面通知','Desktop notifications')}</h2>
      <label className="preference-row"><span>{t('任务完成、待确认和中断','Completed, waiting and interrupted tasks')}</span><input type="checkbox" checked={draft.desktop_notifications} onChange={event=>change('desktop_notifications',event.currentTarget.checked)}/></label>
      <div className="preference-row"><p className="muted">{hostNotifications()?t('macOS 系统通知','macOS notifications'):t('浏览器桌面通知','Browser notifications')}{permission==='granted' ? t(' · 已授权',' · Allowed') : permission==='denied' ? t(' · 已拒绝',' · Blocked') : permission==='unsupported' ? t(' · 不可用',' · Unavailable') : t(' · 待授权',' · Permission required')}</p>
        <button type="button" className="outline" onClick={()=>void testNotification()} disabled={permission==='unsupported'}>{t('测试通知','Test notification')}</button>
      </div>
    </section>
    <section className="settings-section">
      <h2>{t('执行与权限','Execution and permissions')}</h2>
      <p className="preference-row"><span>{t('工作区','Workspace')}</span><span className="muted">{multiuser?t('多人模式 · 按账号隔离','Server · Isolated by account'):t('本机模式','Local mode')}</span></p>
    </section>
    {error&&<p className="error" role="alert">{error}</p>}
    {notice&&<p className="muted" role="status">{notice}</p>}
    <footer><button className="primary" disabled={busy||!value}>{busy?t('正在保存…','Saving…'):t('保存设置','Save preferences')}</button></footer>
  </form>
}
