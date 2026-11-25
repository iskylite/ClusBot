import {useEffect, useState} from 'react'
import {Database, FolderOpen, RefreshCw} from 'lucide-react'
import {api} from './api'
import {displayDate} from './GeneralPreferences'
import './SettingsDiagnostics.css'

type StorageSnapshot = {
  scope?: string
  checked_at_unix_ms?: number
  database?: {
    state?: string
    allocated_bytes?: number
    internally_reusable_bytes?: number
  }
  wal?: {state?: string; bytes?: number}
  automatic_cleanup?: string
}

type StorageState =
  | {status: 'loading'}
  | {status: 'error'}
  | {status: 'ready'; snapshot: StorageSnapshot}

function formatBytes(value: number | undefined, english: boolean) {
  if (typeof value !== 'number' || !Number.isFinite(value) || value < 0) return undefined
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB']
  const index = value < 1 ? 0 : Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1)
  const amount = value / 1024 ** index
  return `${new Intl.NumberFormat(english ? 'en-US' : 'zh-CN', {maximumFractionDigits: 2}).format(amount)} ${units[index]}`
}

export function SettingsDiagnostics({english}: {english: boolean}) {
  const [storage, setStorage] = useState<StorageState>({status: 'loading'})
  const [request, setRequest] = useState(0)
  const t = (zh: string, en: string) => english ? en : zh

  useEffect(() => {
    let active = true
    api<StorageSnapshot>('/settings/data/storage').then(snapshot => {
      if (!active) return
      setStorage(snapshot && typeof snapshot === 'object' ? {status: 'ready', snapshot} : {status: 'error'})
    }).catch(() => {if (active) setStorage({status: 'error'})})
    return () => {active = false}
  }, [request])

  function refresh() {
    if (storage.status === 'loading') return
    setStorage({status: 'loading'})
    setRequest(value => value + 1)
  }

  const snapshot = storage.status === 'ready' ? storage.snapshot : undefined
  const unavailable = t('暂不可用', 'Unavailable')
  const allocated = snapshot?.database?.state === 'available' ? formatBytes(snapshot.database.allocated_bytes, english) : undefined
  const reusable = snapshot?.database?.state === 'available' ? formatBytes(snapshot.database.internally_reusable_bytes, english) : undefined
  const wal = snapshot?.wal?.state === 'absent' ? t('当前不存在', 'Not present') :
    snapshot?.wal?.state === 'available' ? formatBytes(snapshot.wal.bytes, english) : undefined
  let checkedAt: string | undefined
  if (typeof snapshot?.checked_at_unix_ms === 'number' && Number.isFinite(snapshot.checked_at_unix_ms) && snapshot.checked_at_unix_ms > 0) {
    try {checkedAt = displayDate(snapshot.checked_at_unix_ms, {dateStyle: 'medium', timeStyle: 'medium'})} catch { /* Invalid timestamps have no display value. */ }
  }

  return <>
    <section className="diagnostics-section">
      <h2><FolderOpen size={14} aria-hidden="true"/>{t('工作区', 'Workspace')}</h2>
      <dl className="diagnostics-rows">
        <div className="diagnostics-row">
          <dt>{t('范围', 'Scope')}</dt>
          <dd>{t('个人工作区 · 按账号隔离', 'Personal workspace · isolated by account')}</dd>
        </div>
      </dl>
    </section>
    <section className="diagnostics-section">
      <header className="diagnostics-header">
        <h2><Database size={14} aria-hidden="true"/>{t('存储', 'Storage')}</h2>
        <button type="button" className="outline diagnostics-refresh" onClick={refresh} disabled={storage.status === 'loading'}>
          <RefreshCw size={13} aria-hidden="true"/>{storage.status === 'error' ? t('重试', 'Retry') : t('刷新', 'Refresh')}
        </button>
      </header>
      {storage.status === 'loading' ? <p className="diagnostics-message">{t('正在读取存储信息…', 'Loading storage information…')}</p> :
        storage.status === 'error' ? <p className="diagnostics-message" role="status">{t('无法读取存储信息，请重试。', 'Could not load storage information. Please retry.')}</p> : snapshot && <>
          <dl className="diagnostics-rows">
            <div className="diagnostics-row">
              <dt>{t('主数据库分配空间', 'Main database allocation')}</dt>
              <dd>{allocated ?? unavailable}</dd>
            </div>
            <div className="diagnostics-row">
              <dt>{t('数据库内部可复用空间', 'Space reusable by the database')}</dt>
              <dd>{reusable ?? unavailable}</dd>
            </div>
            <div className="diagnostics-row">
              <dt>{t('WAL 日志文件', 'WAL file')}</dt>
              <dd>{wal ?? unavailable}</dd>
            </div>
            <div className="diagnostics-row">
              <dt>{t('自动清理', 'Automatic cleanup')}</dt>
              <dd>{snapshot.automatic_cleanup === 'not_enabled' ? t('未启用', 'Not enabled') : unavailable}</dd>
            </div>
            <div className="diagnostics-row">
              <dt>{t('检查时间', 'Checked at')}</dt>
              <dd>{checkedAt ?? unavailable}</dd>
            </div>
          </dl>
          <p className="diagnostics-note">{snapshot.scope === 'sqlite_main_and_wal_only' ?
            t('仅统计 SQLite 主数据库和 WAL，不包含其他文件。数据库内部可复用空间不等于可释放的磁盘空间。', 'Only the SQLite main database and WAL are included, not other files. Space reusable by the database is not necessarily disk space that can be reclaimed.') :
            t('统计范围暂不可用。', 'Storage scope is unavailable.')}</p>
        </>}
    </section>
  </>
}
