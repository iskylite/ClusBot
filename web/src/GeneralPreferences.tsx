import {useEffect, useMemo, useState} from 'react'
import {Settings, ShieldCheck} from 'lucide-react'
import './GeneralPreferences.css'
import {api, type Overview} from './api'
import {FeedbackCallout} from './Feedback'
import type {PreferenceKey, PreferenceSnapshot} from './preferencesController'

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
  reduce_motion: false, send_shortcut: 'enter', desktop_notifications: false, revision: 0,
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

function zoneOffset(zone:string) {
  try {
    const raw=new Intl.DateTimeFormat('en-US',{timeZone:zone,timeZoneName:'shortOffset'}).formatToParts().find(part=>part.type==='timeZoneName')?.value
    if(!raw)return ''
    const match=/^GMT(?:([+-])(\d{1,2})(?::(\d{2}))?)?$/.exec(raw)
    if(!match)return raw.replace(/^GMT/,'UTC')
    return match[1] ? `UTC${match[1]}${Number(match[2])}${match[3]&&match[3]!=='00'?`:${match[3]}`:''}` : 'UTC+0'
  } catch {return ''}
}
function zoneLabel(zone:string) {
  const offset=zoneOffset(zone)
  return offset?`${zone} (${offset})`:zone
}

export function GeneralPreferencesPage({state,onChange,onRetry,onResolve,multiuser,view='general',modelConcurrency}: {
  state:PreferenceSnapshot
  onChange:<K extends PreferenceKey>(key:K, value:Preferences[K])=>void
  onRetry:()=>void
  onResolve:(keepMine:boolean)=>void
  multiuser:boolean
  view?:'general'|'execution'
  modelConcurrency?:number
}) {
  const [notice,setNotice] = useState('')
  const [notificationError,setNotificationError] = useState('')
  const [permission,setPermission] = useState('')
  const value=state.value
  const draft=value??defaults
  const english = uiLanguage(value?.language) === 'en-US'
  const t = (zh:string,en:string) => label(zh,en,english)
  useEffect(() => {void notificationPermission().then(setPermission).catch(() => setPermission('unsupported'))}, [])

  async function testNotification() {
    setNotificationError('')
    setNotice('')
    try {
      const status = await notificationPermission(true)
      setPermission(status)
      if (status === 'granted' && await notify('test',crypto.randomUUID())) {
        setNotice(t('系统已接收测试通知','Test notification submitted to the system'))
      } else {
        setNotificationError(t('未能投递测试通知，请检查系统或浏览器的通知权限。','Check this app’s notification permissions and try again.'))
      }
    } catch (error) {setNotificationError(message(error))}
  }

  const systemZone = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'
  const zones=useMemo(()=>Array.from(new Set(['Asia/Shanghai','UTC',systemZone,...(Intl.supportedValuesOf?.('timeZone')??['America/Los_Angeles','America/New_York','Europe/London','Europe/Paris','Asia/Tokyo','Australia/Sydney'])])),[systemZone])
  const availableZones=zones.includes(draft.timezone)||draft.timezone==='system'?zones:[draft.timezone,...zones]
  const execution = view==='execution'
  const conflictLabels:Record<PreferenceKey,string>={
    language:t('界面语言','Interface language'),timezone:t('显示时区','Display time zone'),appearance:t('外观','Appearance'),
    reduce_motion:t('减弱动态效果','Reduce motion'),send_shortcut:t('发送快捷键','Send shortcut'),desktop_notifications:t('桌面通知','Desktop notifications'),
  }
  return <div className={'preferences-form'+(execution?' preferences-execution':'')}>
    <h1 className="settings-sr-only">{execution?t('执行与权限','Execution and access'):t('常规','General')}</h1>
    {execution ? <section className="settings-section">
      <h2><ShieldCheck size={14} aria-hidden="true"/>{t('执行与权限边界','Execution and permissions')}</h2>
      <div className="preference-row"><div className="preference-copy"><strong>{t('跨提供方数据出域确认','Cross-provider data transfer confirmation')}</strong><small>{t('配置跨提供方的模型策略时须确认发送的数据范围','Confirm what data can be sent when configuring cross-provider model routing')}</small></div><span className="preference-value">{t('需要确认','Confirmation required')}</span></div>
      <div className="preference-row"><div className="preference-copy"><strong>{t('工作区与账号存储隔离','Workspace and account storage')}</strong>{multiuser&&<small>{t('按账号独立存储对话、附件与运行回执','Conversations, attachments, and run receipts are stored separately by account')}</small>}</div><span className="preference-value">{multiuser?t('多人服务器模式 · 已启用','Multi-user server · Enabled'):t('本机模式','Local mode')}</span></div>
    </section> : <section className="settings-section">
      <h2><Settings size={14} aria-hidden="true"/>{t('常规偏好','General preferences')}</h2>
      <div className="preference-row preference-select-row"><div className="preference-copy"><label htmlFor="pref-language">{t('界面语言','Interface language')}</label></div><select id="pref-language" value={draft.language} disabled={!value} onChange={event=>onChange('language',event.currentTarget.value as Preferences['language'])}>
        <option value="system">{t('跟随系统','System')}</option><option value="zh-CN">简体中文</option><option value="en-US">English</option>
      </select></div>
      <div className="preference-row preference-select-row"><div className="preference-copy"><label htmlFor="pref-timezone">{t('显示时区','Display time zone')}</label><small>{t('仅影响界面展示','Only affects displayed times')}</small></div><select id="pref-timezone" value={draft.timezone} disabled={!value} onChange={event=>onChange('timezone',event.currentTarget.value)}>
        <option value="system">{t('跟随系统','System')} · {zoneLabel(systemZone)}</option>
        {availableZones.map(zone=><option key={zone} value={zone}>{zone===draft.timezone?zoneLabel(zone):zone}</option>)}
      </select></div>
      <div className="preference-row preference-select-row"><div className="preference-copy"><label htmlFor="pref-appearance">{t('外观','Appearance')}</label></div><select id="pref-appearance" value={draft.appearance} disabled={!value} onChange={event=>onChange('appearance',event.currentTarget.value as Preferences['appearance'])}>
        <option value="system">{t('跟随系统','System')}</option><option value="light">{t('浅色','Light')}</option><option value="dark">{t('深色','Dark')}</option>
      </select></div>
      <div className="preference-row"><div className="preference-copy"><label htmlFor="pref-motion">{t('减弱动态效果','Reduce motion')}</label></div><span className="preference-switch"><input id="pref-motion" type="checkbox" disabled={!value} checked={draft.reduce_motion} onChange={event=>onChange('reduce_motion',event.currentTarget.checked)}/><span aria-hidden="true"/></span></div>
      <div className="preference-row preference-select-row"><div className="preference-copy"><label htmlFor="pref-send-shortcut">{t('发送快捷键','Send shortcut')}</label></div><select id="pref-send-shortcut" value={draft.send_shortcut} disabled={!value} onChange={event=>onChange('send_shortcut',event.currentTarget.value as Preferences['send_shortcut'])}>
        <option value="enter">Enter</option><option value="mod_enter">Ctrl / ⌘ + Enter</option>
      </select></div>
      <div className="preference-row"><div className="preference-copy"><label htmlFor="pref-notifications">{t('桌面通知','Desktop notifications')}</label><small>{t('任务完成、待确认或中断时发送提醒','Notify when tasks complete, need attention or are interrupted')}</small></div><span className="preference-switch"><input id="pref-notifications" type="checkbox" disabled={!value} checked={draft.desktop_notifications} onChange={event=>onChange('desktop_notifications',event.currentTarget.checked)}/><span aria-hidden="true"/></span></div>
      <div className="preference-row preference-notification-status"><p className="muted">{hostNotifications()?t('macOS 系统通知','macOS notifications'):t('浏览器桌面通知','Browser notifications')}{permission==='granted' ? t(' · 已授权',' · Allowed') : permission==='denied' ? t(' · 已拒绝',' · Blocked') : permission==='unsupported' ? t(' · 不可用',' · Unavailable') : t(' · 待授权',' · Permission required')}</p>
        <button type="button" className="outline" onClick={()=>void testNotification()} disabled={permission==='unsupported'}>{t('测试通知','Test notification')}</button>
      </div>
    </section>}
    {state.status==='saving'&&<FeedbackCallout tone="info" compact role="status" message={t('正在保存…','Saving…')}/>}
    {state.status==='error'&&<FeedbackCallout tone="error" role="alert"
      message={(value?t('未能保存设置，当前选择已保留。','Could not save preferences. Your choices are retained.'):t('无法读取偏好设置。','Could not load preferences.'))+(state.error?` ${state.error}`:'')}
      actions={<button type="button" onClick={onRetry}>{t('重试','Retry')}</button>}/>}
    {state.status==='conflict'&&<FeedbackCallout tone="warning" role="alert"
      title={t('其他会话也修改了这些设置','Settings changed in another session')}
      message={`${state.conflicts.map(key=>conflictLabels[key]).join('、')}。${t('当前选择尚未保存。','Your choices are not saved yet.')}`}
      actions={<><button type="button" onClick={()=>onResolve(true)}>{t('保留本次选择','Keep my choices')}</button><button type="button" onClick={()=>onResolve(false)}>{t('使用其他会话设置','Use other session’s settings')}</button></>}/>}
    {notificationError&&<FeedbackCallout tone="error" compact role="alert" message={notificationError}/>}
    {notice&&<FeedbackCallout tone="success" compact role="status" message={notice}/>}
  </div>
}
