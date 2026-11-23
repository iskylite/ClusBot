import { useEffect, useRef, useState, type FormEvent, type MouseEvent, type ReactNode } from 'react'
import { Check, Clock, Copy, KeyRound, Lock, Pencil, RotateCw, ShieldCheck, UserPlus, Users, UserX, X } from 'lucide-react'
import clusbotIcon from './clusbot-brand.svg'
import { displayDate } from './GeneralPreferences'
import { FeedbackCallout } from './Feedback'
import './account-ui.css'

export type Account = {
  principal_id: string
  username: string
  display_name: string
  avatar: string
  status: string
  is_superadmin: boolean
  must_change_password: boolean
  created_at?: number
  last_login_at?: number
}

export type Session = {
  authenticated: boolean
  mode: 'local' | 'server'
  user?: Account
  capabilities?: { manage_users: boolean }
}

type AccountSection = 'profile' | 'password' | 'users'

// A 401 from a password change means the current password was incorrect; it
// does not invalidate the session. Keep account-form errors local to the form.
async function accountRequest<T>(path: string, data?: unknown, method = data === undefined ? 'GET' : 'POST'): Promise<T> {
  const response = await fetch('/api/v1' + path, {
    method,
    headers: data === undefined ? {} : { 'Content-Type': 'application/json' },
    body: data === undefined ? undefined : JSON.stringify(data),
  })
  if (!response.ok) {
    const body = await response.json().catch(() => null)
    if (response.status === 401 && !['/session', '/bootstrap', '/me/password'].includes(path)) {
      window.dispatchEvent(new Event('clusbot:logout'))
    }
    throw new Error(body?.message || '请求未完成，请稍后重试')
  }
  return response.status === 204 ? undefined as T : response.json()
}

const errorMessage = (error: unknown) => error instanceof Error ? error.message : '请求未完成，请稍后重试'
const passwordValid = (value: string) => {
  const bytes = new TextEncoder().encode(value).length
  return bytes >= 12 && bytes <= 1024
}
const usernamePattern = '[A-Za-z0-9][A-Za-z0-9._-]{2,63}'
const singleCharAvatar = (value: string) => Array.from(value.trim())[0] || ''
export const avatarText = (account?: { avatar?: string; display_name?: string; username?: string }) =>
  singleCharAvatar(account?.avatar || '') || singleCharAvatar(account?.display_name || '') || singleCharAvatar(account?.username || '') || '?'
const accountStatus = (account: Account) => account.status === 'disabled' ? '已停用' :
  account.status === 'pending' ? '待激活' : account.must_change_password ? '临时密码待改密' : '正常'
const temporaryPassword = () => Array.from(crypto.getRandomValues(new Uint8Array(16)), byte => byte.toString(16).padStart(2, '0')).join('')
const accountDate = (value: number) => {
  const parts = new Intl.DateTimeFormat('en-CA', {
    year: 'numeric', month: '2-digit', day: '2-digit', timeZone: document.documentElement.dataset.timezone,
  }).formatToParts(value)
  const part = (type: string) => parts.find(item => item.type === type)?.value ?? ''
  return `${part('year')}-${part('month')}-${part('day')}`
}
const accountLastLogin = (value: number) => {
  const day = accountDate(value) === accountDate(Date.now()) ? '今天' : accountDate(value)
  return `${day} ${displayDate(value, { hour: '2-digit', minute: '2-digit', hourCycle: 'h23' })}`
}

function Brand({ title, subtitle }: { title: string; subtitle: string }) {
  return <div className="acct-brand">
    <span className="acct-brand-mark" aria-hidden="true"><img src={clusbotIcon} alt=""/></span>
    <div><h1>{title}</h1><p>{subtitle}</p></div>
  </div>
}

function AccountAvatar({ account }: { account: Account }) {
  const tone = account.status === 'disabled' ? 'disabled' : accountStatus(account) !== '正常' ? 'pending' : account.is_superadmin ? 'admin' : 'member'
  return <span className={'acct-avatar acct-avatar-' + tone} aria-hidden="true">{avatarText(account)}</span>
}

function Feedback({ error, notice }: { error?: string; notice?: string }) {
  return <>{error && <FeedbackCallout tone="error" compact role="alert" message={error}/>}{notice && <FeedbackCallout tone="success" compact role="status" message={notice}/>}</>
}

export function ServerLogin({ onLogin }: { onLogin: (session: Session) => void }) {
  const [bootstrapRequired, setBootstrapRequired] = useState<boolean>()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [confirmation, setConfirmation] = useState('')
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)
  const [attempt, setAttempt] = useState(0)

  useEffect(() => {
    let active = true
    setError('')
    accountRequest<{ required: boolean }>('/bootstrap').then(value => {
      if (active) setBootstrapRequired(value.required)
    }).catch(error => { if (active) setError(errorMessage(error)) })
    return () => { active = false }
  }, [attempt])

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (busy || bootstrapRequired === undefined) return
    setError(''); setNotice('')
    if (bootstrapRequired && !passwordValid(password)) { setError('密码须为 12–1024 字节。'); return }
    if (bootstrapRequired && password !== confirmation) { setError('两次输入的密码不一致。'); return }
    setBusy(true)
    let created = false
    try {
      if (bootstrapRequired) {
        await accountRequest('/bootstrap', { username: username.trim(), password })
        created = true
        setBootstrapRequired(false)
      }
      await accountRequest('/session', { username: username.trim(), password })
      const session = await accountRequest<Session>('/session')
      if (!session.authenticated || !session.user) throw new Error('登录未完成，请重新登录。')
      onLogin(session)
    } catch (error) {
      setError(errorMessage(error))
      if (created) setNotice('超级管理员已创建，请使用刚设置的密码登录。')
    } finally { setBusy(false) }
  }

  if (bootstrapRequired === undefined) return <main className="acct-login-shell"><div className="acct-login-card" role={error ? 'alert' : 'status'}>
    {error ? <><p className="acct-error">{error}</p><button type="button" className="acct-button" onClick={() => setAttempt(value => value + 1)}>重试</button></> : '正在检查账号状态…'}
  </div></main>

  return <main className="acct-login-shell">
    <form className={'acct-login-card' + (bootstrapRequired ? ' acct-bootstrap-card' : '')} onSubmit={submit} method="post" action={bootstrapRequired ? '/api/v1/bootstrap' : '/api/v1/session'}>
      <Brand title={bootstrapRequired ? '首次部署 · 创建首位超级管理员' : 'ClusBot · 账号认证'} subtitle={bootstrapRequired ? '创建成功后将关闭初始化入口，无需安装码' : '使用账号与密码登录 ClusBot 个人助手'}/>
      <label className="acct-field"><span>{bootstrapRequired ? '超级管理员用户名' : '用户名'}</span>
        <input name="username" autoComplete="username" value={username} onChange={event => setUsername(event.target.value)} required maxLength={64} pattern={bootstrapRequired ? usernamePattern : undefined} title={bootstrapRequired ? '3–64 位字母、数字或 ._-，首位须为字母或数字' : undefined} disabled={busy}/>
      </label>
      <label className="acct-field"><span>{bootstrapRequired ? '设置密码' : '密码'}</span>
        <input name="password" type="password" autoComplete={bootstrapRequired ? 'new-password' : 'current-password'} value={password} onChange={event => setPassword(event.target.value)} required maxLength={1024} disabled={busy}/>
      </label>
      {bootstrapRequired && <label className="acct-field"><span>确认密码</span>
        <input name="password_confirmation" type="password" autoComplete="new-password" value={confirmation} onChange={event => setConfirmation(event.target.value)} required maxLength={1024} disabled={busy}/>
      </label>}
      <Feedback error={error} notice={notice}/>
      <button type="submit" className="acct-primary acct-login-submit" disabled={busy}>{bootstrapRequired && <ShieldCheck size={15}/>} {busy ? '请稍候…' : bootstrapRequired ? '创建首位超级管理员并进入' : '登录进入个人助手'}</button>
    </form>
  </main>
}

function PasswordFields({ current, setCurrent, password, setPassword, confirmation, setConfirmation, busy }: {
  current: string; setCurrent: (value: string) => void; password: string; setPassword: (value: string) => void
  confirmation: string; setConfirmation: (value: string) => void; busy: boolean
}) {
  return <>
    <label className="acct-field"><span>当前密码</span><input name="current_password" type="password" autoComplete="current-password" data-acct-autofocus value={current} onChange={event => setCurrent(event.target.value)} required maxLength={1024} disabled={busy}/></label>
    <label className="acct-field"><span>新密码</span><input name="new_password" type="password" autoComplete="new-password" value={password} onChange={event => setPassword(event.target.value)} required maxLength={1024} disabled={busy} aria-label="新密码" aria-describedby="acct-password-help"/><small id="acct-password-help">密码须为 12–1024 字节。</small></label>
    <label className="acct-field"><span>确认新密码</span><input name="password_confirmation" type="password" autoComplete="new-password" value={confirmation} onChange={event => setConfirmation(event.target.value)} required maxLength={1024} disabled={busy}/></label>
  </>
}

function PasswordForm({ onChanged, forced = false }: { onChanged: () => void; forced?: boolean }) {
  const [current, setCurrent] = useState('')
  const [password, setPassword] = useState('')
  const [confirmation, setConfirmation] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (busy) return
    setError('')
    if (!passwordValid(password)) { setError('新密码须为 12–1024 字节。'); return }
    if (password === current) { setError('新密码不能与当前密码相同。'); return }
    if (password !== confirmation) { setError('两次输入的新密码不一致。'); return }
    setBusy(true)
    try { await accountRequest('/me/password', { current_password: current, new_password: password }); onChanged() }
    catch (error) { setError(errorMessage(error)) }
    finally { setBusy(false) }
  }
  return <form className="acct-form" onSubmit={submit} method="post" action="/api/v1/me/password">
    <p className="acct-muted">{forced ? '请先修改临时密码。修改成功后需重新登录。' : '修改成功后所有登录会话将失效，请重新登录。'}</p>
    <PasswordFields current={current} setCurrent={setCurrent} password={password} setPassword={setPassword} confirmation={confirmation} setConfirmation={setConfirmation} busy={busy}/>
    <Feedback error={error}/>
    <div className="acct-actions"><button type="submit" className="acct-primary" disabled={busy}>{busy ? '正在保存…' : '修改密码'}</button></div>
  </form>
}

export function ForcedPasswordChange({ account, onLogout }: { account: Account; onLogout: () => void }) {
  const [error, setError] = useState('')
  async function logout() {
    try { await accountRequest('/session', undefined, 'DELETE'); onLogout() }
    catch (error) { setError(errorMessage(error)) }
  }
  return <main className="acct-login-shell"><section className="acct-login-card acct-bootstrap-card">
    <Brand title="修改临时密码" subtitle={`${account.display_name}（${account.username}）`}/>
    <PasswordForm forced onChanged={onLogout}/>
    <Feedback error={error}/>
    <button type="button" className="acct-text-button" onClick={logout}>退出登录</button>
  </section></main>
}

function Modal({ title, icon, children, onClose, className = '', busy = false }: { title: string; icon?: ReactNode; children: ReactNode | ((close: () => void) => ReactNode); onClose: () => void; className?: string; busy?: boolean }) {
  const ref = useRef<HTMLDialogElement>(null)
  useEffect(() => {
    const dialog = ref.current
    if (dialog && !dialog.open) {
      dialog.showModal()
      dialog.querySelector<HTMLElement>('[data-acct-autofocus]')?.focus()
    }
  }, [])
  const close = () => ref.current?.close()
  return <dialog ref={ref} className={'acct-modal ' + className} aria-labelledby="acct-modal-title" onClose={onClose} onCancel={event => { if (busy) event.preventDefault() }}>
    <header className="acct-modal-header"><h2 id="acct-modal-title">{icon}{title}</h2><button type="button" className="acct-icon-button" aria-label="关闭" disabled={busy} onClick={close}><X size={17}/></button></header>
    {typeof children === 'function' ? children(close) : children}
  </dialog>
}

function ProfileDialog({ account, onClose, onSaved }: { account: Account; onClose: () => void; onSaved: (account: Account) => void }) {
  const [displayName, setDisplayName] = useState(account.display_name)
  const [avatar, setAvatar] = useState(() => singleCharAvatar(account.avatar))
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  async function submit(event: FormEvent<HTMLFormElement>, close: () => void) {
    event.preventDefault(); if (busy) return
    setBusy(true); setError('')
    try {
      const updated = await accountRequest<Account>('/me/profile', { display_name: displayName.trim(), avatar: singleCharAvatar(avatar) }, 'PATCH')
      onSaved(updated); close()
    } catch (error) { setError(errorMessage(error)) }
    finally { setBusy(false) }
  }
  return <Modal title="编辑头像与姓名" icon={<Pencil size={16}/>} onClose={onClose} busy={busy}>
    {close => <form className="acct-modal-form" onSubmit={event => void submit(event, close)} method="post" action="/api/v1/me/profile">
      <label className="acct-field"><span>显示姓名</span><input name="display_name" value={displayName} onChange={event => setDisplayName(event.target.value)} required maxLength={120} disabled={busy} data-acct-autofocus/></label>
      <label className="acct-field"><span>头像标识</span><input name="avatar" value={avatar} onChange={event => setAvatar(singleCharAvatar(event.target.value))} maxLength={16} placeholder="单字标识" disabled={busy}/></label>
      <Feedback error={error}/>
      <div className="acct-modal-actions"><button type="button" className="acct-button" onClick={close} disabled={busy}>取消</button><button className="acct-primary" type="submit" disabled={busy}>{busy ? '正在保存…' : '保存资料'}</button></div>
    </form>}
  </Modal>
}

function CreateDialog({ onClose, onCreated }: { onClose: () => void; onCreated: () => void }) {
  const [displayName, setDisplayName] = useState('')
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState(temporaryPassword)
  const [mustChange, setMustChange] = useState(true)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [copyStatus, setCopyStatus] = useState<'copied' | 'failed'>()
  useEffect(() => {
    if (!copyStatus) return
    const timer = window.setTimeout(() => setCopyStatus(undefined), 2400)
    return () => window.clearTimeout(timer)
  }, [copyStatus])
  async function copyPassword() {
    try {
      await navigator.clipboard.writeText(password)
      setCopyStatus('copied')
    } catch {
      setCopyStatus('failed')
    }
  }
  async function submit(event: FormEvent<HTMLFormElement>, close: () => void) {
    event.preventDefault(); if (busy) return
    setError('')
    if (!passwordValid(password)) { setError('初始密码须为 12–1024 字节。'); return }
    setBusy(true)
    try {
      await accountRequest('/admin/users', { display_name: displayName.trim(), username: username.trim(), password, is_superadmin: false, must_change_password: mustChange })
      onCreated(); close()
    } catch (error) { setError(errorMessage(error)) }
    finally { setBusy(false) }
  }
  return <Modal title="创建新成员" icon={<UserPlus size={16}/>} onClose={onClose} className="acct-create" busy={busy}>
    {close => <form className="acct-modal-form" onSubmit={event => void submit(event, close)} method="post" action="/api/v1/admin/users">
      <label className="acct-field"><span>显示姓名</span><input name="display_name" placeholder="周工 / Alex Zhou" value={displayName} onChange={event => setDisplayName(event.target.value)} required maxLength={120} disabled={busy} data-acct-autofocus/></label>
      <label className="acct-field"><span>登录用户名</span><input name="username" placeholder="alex.zhou" value={username} onChange={event => setUsername(event.target.value)} required pattern={usernamePattern} title="3–64 位字母、数字或 ._-，首位须为字母或数字" maxLength={64} autoComplete="off" disabled={busy}/></label>
      <div className="acct-field">
        <label htmlFor="acct-create-password">初始临时密码</label><small id="acct-create-password-help" className="acct-visually-hidden">密码须为 12–1024 字节。</small>
        <div className="acct-password-input">
          <input id="acct-create-password" name="password" type="text" autoComplete="new-password" value={password} onChange={event => { setPassword(event.target.value); setCopyStatus(undefined) }} required maxLength={1024} disabled={busy} aria-describedby="acct-create-password-help"/>
          <button type="button" className="acct-icon-button" aria-label="复制临时密码" title="复制临时密码" disabled={busy || !password} onClick={() => void copyPassword()}>{copyStatus === 'copied' ? <Check size={14} aria-hidden="true"/> : <Copy size={14} aria-hidden="true"/>}</button>
          <span className="acct-copy-status" role="status">{copyStatus === 'copied' ? '已复制' : copyStatus === 'failed' ? '复制失败' : ''}</span>
        </div>
      </div>
      <label className="acct-field"><span>账号角色（全站唯一超级管理员由部署初始化确定）</span><span className="acct-readonly-role"><Lock size={13}/><input value="普通用户" readOnly aria-readonly="true"/><span className="acct-badge">固定角色</span></span></label>
      <label className="acct-checkbox"><input type="checkbox" name="must_change_password" checked={mustChange} onChange={event => setMustChange(event.target.checked)} disabled={busy}/><span>要求该成员首次登录时强制修改密码</span></label>
      <Feedback error={error}/>
      <div className="acct-modal-actions"><button type="button" className="acct-button" onClick={close} disabled={busy}>取消</button><button className="acct-primary" type="submit" disabled={busy}><Check size={13}/>{busy ? '正在创建…' : '创建用户账号'}</button></div>
    </form>}
  </Modal>
}

type Confirmation = 'reset' | 'disable' | 'enable'
type MemberAction = 'edit' | 'reset' | 'disable' | 'enable'
function ConfirmDialog({ action, account, onClose, onConfirm, busy, error }: { action: Confirmation; account: Account; onClose: () => void; onConfirm: (close: () => void) => void; busy: boolean; error: string }) {
  const title = action === 'reset' ? '确认重置密码？' : action === 'disable' ? '确认停用账号？' : '确认恢复账号？'
  const detail = action === 'reset' ? `重置后 ${account.display_name} 的现有会话会失效，下次登录时须修改临时密码。` : action === 'disable' ? `停用后 ${account.display_name} 将无法登录，现有会话立即失效。` : `恢复后 ${account.display_name} 可使用现有密码登录。`
  return <Modal title={title} onClose={onClose} className="acct-confirm" busy={busy}>
    {close => <><div className="acct-confirm-body"><p>{detail}</p><Feedback error={error}/></div>
    <div className="acct-modal-actions"><button type="button" className="acct-button" onClick={close} disabled={busy} data-acct-autofocus>取消</button><button type="button" className={action === 'disable' ? 'acct-danger' : 'acct-primary'} disabled={busy} onClick={() => onConfirm(close)}>{busy ? '请稍候…' : action === 'reset' ? '确认重置密码' : action === 'disable' ? '确认停用' : '确认恢复'}</button></div></>}
  </Modal>
}

function MemberDrawer({ member, action, onClose, onUpdated }: { member: Account; action: MemberAction; onClose: () => void; onUpdated: (account: Account) => void }) {
  const [displayName, setDisplayName] = useState(member.display_name)
  const [avatar, setAvatar] = useState(() => singleCharAvatar(member.avatar))
  const [temporaryPassword, setTemporaryPassword] = useState('')
  const [confirmation, setConfirmation] = useState<Confirmation>()
  const [error, setError] = useState('')
  const [confirmError, setConfirmError] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)
  const headingRef = useRef<HTMLHeadingElement>(null)
  const passwordRef = useRef<HTMLInputElement>(null)
  const statusButtonRef = useRef<HTMLButtonElement>(null)
  const drawerBusy = busy && !confirmation
  useEffect(() => { setDisplayName(member.display_name); setAvatar(singleCharAvatar(member.avatar)); setTemporaryPassword(''); setError(''); setNotice('') }, [member.principal_id])
  useEffect(() => {
    if (action === 'reset') passwordRef.current?.focus()
    else if (action === 'disable' || action === 'enable') { statusButtonRef.current?.focus(); setConfirmation(action) }
    else headingRef.current?.focus()
  }, [action, member.principal_id])
  async function saveProfile(event: FormEvent<HTMLFormElement>) {
    event.preventDefault(); if (busy) return
    setBusy(true); setError(''); setNotice('')
    try {
      const updated = await accountRequest<Account>('/admin/users/' + encodeURIComponent(member.principal_id), { display_name: displayName.trim(), avatar: singleCharAvatar(avatar) }, 'PATCH')
      onUpdated(updated); setNotice('资料已保存。')
    } catch (error) { setError(errorMessage(error)) }
    finally { setBusy(false) }
  }
  function requestReset(event: FormEvent<HTMLFormElement>) {
    event.preventDefault(); setError(''); setNotice('')
    if (!passwordValid(temporaryPassword)) { setError('临时密码须为 12–1024 字节。'); return }
    setConfirmError(''); setConfirmation('reset')
  }
  async function confirm(close: () => void) {
    if (!confirmation || busy) return
    setBusy(true); setConfirmError('')
    try {
      if (confirmation === 'reset') {
        await accountRequest('/admin/users/' + encodeURIComponent(member.principal_id) + '/password', { password: temporaryPassword })
        setTemporaryPassword(''); onUpdated({ ...member, must_change_password: true, status: member.status === 'pending' ? 'active' : member.status }); setNotice('临时密码已重置。')
      } else {
        const updated = await accountRequest<Account>('/admin/users/' + encodeURIComponent(member.principal_id), { status: confirmation === 'disable' ? 'disabled' : 'active' }, 'PATCH')
        onUpdated(updated); setNotice(confirmation === 'disable' ? '账号已停用。' : '账号已恢复。')
      }
      close()
    } catch (error) { setConfirmError(errorMessage(error)) }
    finally { setBusy(false) }
  }
  return <aside className="acct-drawer" aria-labelledby="acct-drawer-title">
    <header className="acct-drawer-header"><AccountAvatar account={member}/><div><h2 id="acct-drawer-title" ref={headingRef} tabIndex={-1}>{member.display_name} (@{member.username})</h2><p>普通用户 · {accountStatus(member)} · 独立存储空间</p></div><button type="button" className="acct-icon-button" aria-label="关闭成员详情" onClick={onClose}><X size={17}/></button></header>
    <div className="acct-drawer-sections">
      <form className="acct-drawer-section" onSubmit={saveProfile} method="post" action={'/api/v1/admin/users/' + encodeURIComponent(member.principal_id)}>
        <h3>基本资料与账号角色</h3>
        <div className="acct-field-row"><label className="acct-field"><span>显示姓名</span><input name="display_name" value={displayName} onChange={event => setDisplayName(event.target.value)} required maxLength={120} disabled={drawerBusy}/></label><label className="acct-field"><span>头像标识</span><input name="avatar" value={avatar} onChange={event => setAvatar(singleCharAvatar(event.target.value))} maxLength={16} placeholder="单字标识" disabled={drawerBusy}/></label></div>
        <label className="acct-field"><span>账号角色</span><span className="acct-readonly-role"><Lock size={13}/><input value="普通用户" readOnly aria-readonly="true"/><span className="acct-badge">全站唯一超管 · 不可变更</span></span></label>
        <div className="acct-actions"><button type="submit" className="acct-primary" disabled={drawerBusy}><Check size={14}/>保存资料</button></div>
      </form>
      <form className="acct-drawer-section" onSubmit={requestReset} method="post" action={'/api/v1/admin/users/' + encodeURIComponent(member.principal_id) + '/password'}>
        <h3>重置临时密码</h3><p className="acct-muted">重置后立即撤销既有登录会话，下次登录须强制改密。</p>
        <label className="acct-field"><span className="acct-visually-hidden">新的临时密码</span><input ref={passwordRef} name="password" type="password" placeholder="输入新的临时密码…" autoComplete="new-password" value={temporaryPassword} onChange={event => setTemporaryPassword(event.target.value)} required maxLength={1024} disabled={drawerBusy}/></label>
        <div className="acct-actions"><button type="submit" className="acct-button" disabled={drawerBusy}><KeyRound size={14}/>确认重置密码</button></div>
      </form>
      <section className={'acct-drawer-section ' + (member.status === 'disabled' ? '' : 'acct-danger-zone')}>
        <h3>{member.status === 'disabled' ? '恢复此成员账号' : '停用此成员账号'}</h3>
        <p className="acct-muted">{member.status === 'disabled' ? '恢复后该成员可以重新登录。' : '停用后即时失效全部会话并禁止登录，已有任务与产物保留。'}</p>
        <div className="acct-actions"><button ref={statusButtonRef} type="button" className={member.status === 'disabled' ? 'acct-button' : 'acct-danger-outline'} disabled={drawerBusy} onClick={() => { setConfirmError(''); setConfirmation(member.status === 'disabled' ? 'enable' : 'disable') }}>{member.status === 'disabled' ? <><RotateCw size={14}/>恢复账号</> : <><UserX size={14}/>停用账号</>}</button></div>
      </section>
      <Feedback error={error} notice={notice}/>
    </div>
    {Boolean(member.created_at || member.last_login_at) && <footer className="acct-drawer-meta">
      <Clock size={12} aria-hidden="true"/>
      <span>{member.created_at ? <>账号创建时间：<time dateTime={new Date(member.created_at).toISOString()}>{accountDate(member.created_at)}</time> · </> : null}
        最近登录：{member.last_login_at ? <time dateTime={new Date(member.last_login_at).toISOString()}>{accountLastLogin(member.last_login_at)}</time> : '无记录'}</span>
    </footer>}
    {confirmation && <ConfirmDialog action={confirmation} account={member} onClose={() => setConfirmation(undefined)} onConfirm={confirm} busy={busy} error={confirmError}/>}
  </aside>
}

export function AccountPage({ account, onAccountChange, onLogout, initialSection, onNavigate }: { account: Account; onAccountChange: (account: Account) => void; onLogout: () => void; initialSection?: AccountSection; onNavigate?: (path: string) => void }) {
  const [users, setUsers] = useState<Account[]>()
  const [loadError, setLoadError] = useState('')
  const [modal, setModal] = useState<'profile' | 'password' | 'create' | undefined>(initialSection === 'profile' || initialSection === 'password' ? initialSection : undefined)
  const [selectedId, setSelectedId] = useState<string>()
  const [memberAction, setMemberAction] = useState<MemberAction>('edit')
  const [refresh, setRefresh] = useState(0)
  const canManage = account.is_superadmin && !account.must_change_password
  const showUsers = canManage && initialSection === 'users'
  useEffect(() => {
    if (!showUsers) return
    let active = true
    setLoadError('')
    accountRequest<Account[]>('/admin/users').then(value => { if (active) setUsers(value) }).catch(error => { if (active) setLoadError(errorMessage(error)) })
    return () => { active = false }
  }, [showUsers, refresh])
  function updated(value: Account) {
    setUsers(current => current?.map(user => user.principal_id === value.principal_id ? value : user))
    if (value.principal_id === account.principal_id) onAccountChange(value)
  }
  const selected = showUsers ? users?.find(user => user.principal_id === selectedId) : undefined
  function openMember(user: Account, action: MemberAction = 'edit') {
    setMemberAction(action)
    setSelectedId(user.principal_id)
  }
  function navigateSection(event: MouseEvent<HTMLAnchorElement>, path: string) {
    if (!onNavigate || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return
    event.preventDefault()
    onNavigate(path)
  }
  return <main className="main account-page">
    <h1 className="acct-visually-hidden">账号与安全</h1>
    <nav className="acct-nav" aria-label="账号设置">
      <a href="/account" aria-current={!showUsers ? 'page' : undefined} onClick={event => navigateSection(event, '/account')}>本人账号</a>
      {canManage && <a href="/account#users" aria-current={showUsers ? 'page' : undefined} onClick={event => navigateSection(event, '/account#users')}>用户管理</a>}
    </nav>
    <div className={'acct-layout' + (selected ? ' has-drawer' : '')}>
      <div className="acct-main-content">
        {!showUsers && <section className="acct-self" aria-label="本人账号">
          <AccountAvatar account={account}/><div className="acct-self-copy"><div><strong>{account.display_name} ({account.username})</strong><span className="acct-badge acct-role">{account.is_superadmin ? '超级管理员' : '普通用户'}</span>{account.is_superadmin && <span className="acct-badge acct-good">最后一位超管受保护</span>}</div><p>修改本人密码成功后当前登录会话失效，须重新登录</p></div>
          <div className="acct-self-actions"><button type="button" className="acct-button" onClick={() => setModal('profile')}><Pencil size={14}/>编辑头像与姓名</button><button type="button" className="acct-button" onClick={() => setModal('password')}><Lock size={14}/>修改本人密码</button></div>
        </section>}
        {showUsers && <section className="acct-users" aria-labelledby="acct-users-title">
          <header className="acct-users-heading"><div><Users size={15}/><h2 id="acct-users-title">全站用户管理{users ? ` (${users.length})` : ''}</h2><span className="acct-badge">唯一超级管理员 + {users ? `${users.filter(user => !user.is_superadmin).length} 位` : ''}普通用户</span></div><button type="button" className="acct-primary" onClick={() => setModal('create')}><UserPlus size={13}/>创建用户</button></header>
          {loadError ? <div><Feedback error={loadError}/><button type="button" className="acct-button" onClick={() => setRefresh(value => value + 1)}>重试</button></div> : !users ? <p className="acct-muted" role="status">正在加载用户…</p> :
          <div className="acct-table-wrap" tabIndex={0} role="region" aria-labelledby="acct-users-title"><table className="acct-table"><thead><tr><th scope="col">头像 · 显示姓名与用户名</th><th scope="col">账号角色</th><th scope="col">账号状态</th><th scope="col">管理操作</th></tr></thead><tbody>
            {users.map(user => {
              const isSelected = user.principal_id === selectedId
              const status = accountStatus(user)
              const tone = status === '正常' ? 'acct-good' : user.status === 'disabled' ? 'acct-disabled' : 'acct-warning'
              return <tr key={user.principal_id} className={isSelected ? 'selected' : ''}>
                <td><div className="acct-person"><AccountAvatar account={user}/><div><strong>{user.display_name}</strong><small>@{user.username}</small></div></div></td>
                <td><span className={'acct-badge ' + (user.is_superadmin ? 'acct-role' : '')}>{user.is_superadmin ? '超级管理员' : '普通用户'}</span></td>
                <td><span className={'acct-badge ' + tone}>{user.status === 'disabled' ? '已停用 · 会话已撤销' : status === '正常' && user.is_superadmin ? '正常 · 受保护' : status}</span></td>
                <td><div className="acct-table-actions">
                  {user.principal_id === account.principal_id ? <button type="button" className="acct-button" onClick={() => setModal('profile')}><Pencil size={13}/>编辑资料</button> :
                    !user.is_superadmin && (isSelected ? <button type="button" className="acct-button acct-member-active" aria-expanded={true} onClick={() => setSelectedId(undefined)}><Pencil size={13}/>管理中</button> :
                      user.status === 'disabled' ? <button type="button" className="acct-button" onClick={() => openMember(user, 'enable')}><RotateCw size={13}/>恢复账号</button> : <>
                        <button type="button" className="acct-button" onClick={() => openMember(user)}><Pencil size={13}/>编辑</button>
                        {!selected && <><button type="button" className="acct-button" onClick={() => openMember(user, 'reset')}><KeyRound size={13}/>重置密码</button>
                          <button type="button" className="acct-button" onClick={() => openMember(user, 'disable')}><UserX size={13}/>停用</button></>}
                      </>)}
                </div></td>
              </tr>
            })}
          </tbody></table></div>}
        </section>}
      </div>
      {selected && !selected.is_superadmin && <MemberDrawer key={selected.principal_id} member={selected} action={memberAction} onClose={() => setSelectedId(undefined)} onUpdated={updated}/>}
    </div>
    {modal === 'profile' && <ProfileDialog account={account} onClose={() => setModal(undefined)} onSaved={updated}/>}
    {modal === 'password' && <Modal title="修改本人密码" icon={<Lock size={16}/>} onClose={() => setModal(undefined)}><div className="acct-modal-form"><PasswordForm onChanged={onLogout}/></div></Modal>}
    {modal === 'create' && canManage && <CreateDialog onClose={() => setModal(undefined)} onCreated={() => setRefresh(value => value + 1)}/>}
  </main>
}
