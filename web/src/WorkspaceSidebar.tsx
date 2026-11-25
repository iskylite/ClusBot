import { Activity, ChevronRight, Layers, MessageSquare, PanelLeft, Settings2, type LucideIcon } from 'lucide-react'
import { avatarText, type Account } from './AccountUI'
import type { Task } from './api'
import clusbotIcon from './clusbot-brand.svg'

type WorkspaceSidebarProps = {
  menu: boolean
  collapsed: boolean
  section: string
  settingsArea: boolean
  personalTaskItems: Task[]
  allTaskCount?: number
  superadmin: boolean
  user?: Account
  t: (zh: string, en: string) => string
  taskReasonIcons: Record<string, LucideIcon>
  taskStatusIcons: Record<Task['status'], LucideIcon>
  onToggleCollapsed: () => void
  onReturnToChat: () => void
  onNavigate: (path: string) => void
  onOpenTask: (task: Task, trigger: HTMLButtonElement) => void
}

export function WorkspaceSidebar({
  menu, collapsed, section, settingsArea, personalTaskItems, allTaskCount, superadmin, user,
  t, taskReasonIcons, taskStatusIcons, onToggleCollapsed, onReturnToChat,
  onNavigate, onOpenTask,
}: WorkspaceSidebarProps) {
  return <aside id="workspace-sidebar" className={'sidebar app-sidebar ' + (menu ? 'open' : '')}>
    <div className="sidebar-masthead">
      <div className="brand sidebar-brand">
        <img src={clusbotIcon} width={38} height={38} alt=""/>
        <span><strong>ClusBot</strong><small>{t('个人运维助手','Operations assistant')}</small></span>
      </div>
    </div>
    <nav className="sidebar-primary-nav" aria-label={t('主导航','Main navigation')}>
      <button type="button" className="nav-item" aria-current={section==='chat'?'page':undefined} aria-label={t('个人助手','Assistant')} title={collapsed?t('个人助手','Assistant'):undefined} onClick={onReturnToChat}>
        <MessageSquare size={16}/><span>{t('个人助手','Assistant')}</span>{personalTaskItems.length>0&&<b className="sidebar-count" aria-hidden="true">{personalTaskItems.length}</b>}
      </button>
      <button type="button" className="nav-item" aria-label={t('任务清单','Task list')} title={collapsed?t('任务清单','Task list'):undefined} aria-current={section==='tasks'||section==='team-tasks'?'page':undefined} onClick={()=>onNavigate(superadmin?'/team-tasks':'/tasks')}>
        <Layers size={16}/><span>{t('任务清单','Task list')}</span>{superadmin&&Boolean(allTaskCount)&&<b className="sidebar-count" aria-hidden="true">{allTaskCount}</b>}
      </button>
    </nav>
    {personalTaskItems.length>0&&<section className="sidebar-items" aria-label={t('当前事项','Current tasks')}>
      <h2>{t('当前事项','Current tasks')} ({personalTaskItems.length})</h2>
      {personalTaskItems.map(task=>{
        const Icon=taskReasonIcons[task.status_reason]??(task.status==='active'?Activity:taskStatusIcons[task.status])
        const status=task.status_reason==='acceptance_required'?t('待验收','Review pending'):
          task.status_reason==='approval_required'?t('待决定','Decision pending'):
          task.status_reason==='benchmark_dispatch_paused'?t('已暂停后续派发','Dispatch paused'):
          task.status==='active'?t('运行中','Running'):task.reason
        return <button key={task.id} type="button" className="sidebar-task" data-status={task.status} data-reason={task.status_reason} onClick={event=>onOpenTask(task,event.currentTarget)}>
          <Icon size={15} aria-hidden="true"/><span><strong>{task.title}</strong><small>{status}</small></span>
        </button>
      })}
    </section>}
    <div className="sidebar-footer">
      <nav className="sidebar-utilities" aria-label={t('侧栏工具','Sidebar controls')}>
        <button type="button" className="nav-item" aria-current={settingsArea?'page':undefined} aria-label={t('设置','Settings')} title={collapsed?t('设置','Settings'):undefined} onClick={()=>onNavigate('/settings')}>
          <Settings2 size={16}/><span>{t('设置','Settings')}</span>
        </button>
        <button type="button" className="nav-item sidebar-collapse" aria-label={collapsed?t('展开侧栏','Expand sidebar'):t('折叠侧栏','Collapse sidebar')} title={collapsed?t('展开侧栏','Expand sidebar'):undefined} aria-expanded={!collapsed} onClick={onToggleCollapsed}>
          <PanelLeft size={16}/><span>{collapsed?t('展开侧栏','Expand sidebar'):t('折叠侧栏','Collapse sidebar')}</span>
        </button>
      </nav>
      {user&&<button type="button" className="sidebar-account" aria-label={t('账号与安全','Account and security')} aria-current={section==='account'?'page':undefined} title={collapsed?t('账号与安全','Account and security'):undefined} onClick={()=>onNavigate('/account')}>
        <span className="sidebar-avatar">{avatarText(user)}</span>
        <span className="sidebar-account-copy"><strong>{user.display_name&&user.display_name!==user.username?user.display_name+' · '+user.username:user.username}</strong>
          <small>{superadmin?t('超级管理员','Administrator'):t('普通用户','Member')}</small></span><ChevronRight size={14}/>
      </button>}
    </div>
  </aside>
}
