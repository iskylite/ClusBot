import { useState, type ReactNode } from 'react'
import { CheckCircle2, CircleAlert, Clock3, Info, LoaderCircle, X } from 'lucide-react'

export type FeedbackTone = 'info' | 'warning' | 'error' | 'success'

const toneIcons = {
  info: Info,
  warning: Clock3,
  error: CircleAlert,
  success: CheckCircle2,
} as const

function isLongMessage(message: string) {
  return message.length > 140 || message.includes('\n')
}

export function FeedbackCallout({
  tone,
  title,
  message,
  role,
  compact = false,
  actions,
  onDismiss,
  className = '',
}: {
  tone: FeedbackTone
  title?: string
  message: string
  role?: 'alert' | 'status'
  compact?: boolean
  actions?: ReactNode
  onDismiss?: () => void
  className?: string
}) {
  const [expanded, setExpanded] = useState(false)
  const Icon = toneIcons[tone]
  const long = isLongMessage(message)
  const resolvedRole = role ?? (tone === 'error' ? 'alert' : 'status')

  return (
    <div
      className={`fb-callout${compact ? ' fb-compact' : ''}${className ? ` ${className}` : ''}`}
      data-tone={tone}
      role={resolvedRole}
    >
      <span className="fb-icon" aria-hidden="true">
        <Icon size={15} />
      </span>
      <div className="fb-body">
        {title && <strong className="fb-title">{title}</strong>}
        <p className={`fb-message${long && !expanded ? ' fb-clamped' : ''}`}>{message}</p>
        {long && (
          <button
            type="button"
            className="fb-expand"
            aria-expanded={expanded}
            onClick={() => setExpanded(prev => !prev)}
          >
            {expanded ? '收起详情' : '展开详情'}
          </button>
        )}
        {actions && <div className="fb-actions">{actions}</div>}
      </div>
      {onDismiss && (
        <button
          type="button"
          className="fb-dismiss"
          aria-label="关闭提示"
          onClick={onDismiss}
        >
          <X size={14} aria-hidden="true" />
        </button>
      )}
    </div>
  )
}

export function ComposerStatusRail({
  tone,
  message,
  role,
  actions,
  onDismiss,
}: {
  tone: 'loading' | 'warning' | 'error'
  message: string
  role?: 'alert' | 'status'
  actions?: ReactNode
  onDismiss?: () => void
}) {
  const [expanded, setExpanded] = useState(false)
  const Icon = tone === 'loading' ? LoaderCircle : tone === 'warning' ? Clock3 : CircleAlert
  const long = isLongMessage(message)
  const resolvedRole = role ?? (tone === 'error' ? 'alert' : 'status')

  return (
    <div className="composer-status-rail" data-tone={tone} role={resolvedRole}>
      <span className="composer-status-icon" aria-hidden="true">
        <Icon size={14} className={tone === 'loading' ? 'spin' : undefined} />
      </span>
      <div className="composer-status-body">
        <span className={`composer-status-text${long && !expanded ? ' fb-clamped' : ''}`}>
          {message}
        </span>
        {long && (
          <button
            type="button"
            className="fb-expand"
            aria-expanded={expanded}
            onClick={() => setExpanded(prev => !prev)}
          >
            {expanded ? '收起' : '详情'}
          </button>
        )}
      </div>
      {actions && <div className="composer-status-actions">{actions}</div>}
      {onDismiss && (
        <button
          type="button"
          className="composer-status-dismiss"
          aria-label="关闭提示"
          onClick={onDismiss}
        >
          <X size={13} aria-hidden="true" />
        </button>
      )}
    </div>
  )
}

export type AssistantNoticeTone = 'info' | 'warning' | 'error' | 'success' | 'resolved'

export function AssistantNoticeMessage({
  iconSrc,
  tone,
  badge,
  title,
  message,
  detail,
  actions,
  onDismiss,
  role,
}: {
  iconSrc?: string
  tone: AssistantNoticeTone
  badge: string
  title: string
  message: string
  detail?: string
  actions?: ReactNode
  onDismiss?: () => void
  role?: 'alert' | 'status'
}) {
  const [expanded, setExpanded] = useState(false)
  const Icon =
    tone === 'error'
      ? CircleAlert
      : tone === 'warning'
        ? Clock3
        : tone === 'success' || tone === 'resolved'
          ? CheckCircle2
          : Info
  const long = isLongMessage(message)
  const resolvedRole = role ?? (tone === 'error' ? 'alert' : 'status')

  return (
    <article
      className="message message-assistant assistant-notice-message"
      data-tone={tone}
      role={resolvedRole}
    >
      <span>
        {iconSrc && <img src={iconSrc} alt="" />}
        <strong>ClusBot</strong>
        <span className="assistant-notice-badge" data-tone={tone}>
          <Icon size={11} aria-hidden="true" />
          {badge}
        </span>
      </span>
      <div className="assistant-notice-card" data-tone={tone}>
        <div className="assistant-notice-head">
          <strong>{title}</strong>
          {onDismiss && (
            <button
              type="button"
              className="fb-dismiss"
              aria-label="关闭提示"
              onClick={onDismiss}
            >
              <X size={14} aria-hidden="true" />
            </button>
          )}
        </div>
        <p className={`fb-message${long && !expanded ? ' fb-clamped' : ''}`}>{message}</p>
        {detail && <p className="assistant-notice-detail">{detail}</p>}
        {long && (
          <button
            type="button"
            className="fb-expand"
            aria-expanded={expanded}
            onClick={() => setExpanded(prev => !prev)}
          >
            {expanded ? '收起详情' : '展开详情'}
          </button>
        )}
        {actions && tone !== 'resolved' && <div className="assistant-notice-actions">{actions}</div>}
      </div>
    </article>
  )
}
