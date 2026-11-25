import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import test from 'node:test'
import React from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import ts from 'typescript'

const source = (await readFile(new URL('../src/WorkspaceSidebar.tsx', import.meta.url), 'utf8'))
  .replace(/^import clusbotIcon .*\n/m, "const clusbotIcon = 'clusbot-brand.svg'\n")
  .replace(/^import \{ avatarText, type Account \} from '\.\/AccountUI'\n/m,
    "const avatarText = (account?: { avatar?: string; display_name?: string; username?: string }) => Array.from((account?.avatar || account?.display_name || account?.username || '?').trim())[0] || '?'\n")
  .replace(/^import type \{ Task \} from '\.\/api'\n/m, '')
const compiled = ts.transpileModule(source, { compilerOptions: {
  target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext, jsx: ts.JsxEmit.ReactJSX,
} }).outputText
const js = compiled.replace(/from ['"](react(?:\/jsx-runtime)?|lucide-react)['"]/g,
  (_, specifier) => `from '${import.meta.resolve(specifier)}'`)
const { WorkspaceSidebar } = await import('data:text/javascript;base64,' + Buffer.from(js).toString('base64'))

const zh = (text) => text
const noop = () => {}
const adminUser = {
  principal_id: 'admin',
  username: 'admin',
  display_name: '林工',
  avatar: '林',
  status: 'active',
  is_superadmin: true,
  must_change_password: false,
}
const memberUser = {
  ...adminUser,
  principal_id: 'member',
  username: 'member',
  display_name: '小李',
  avatar: '李',
  is_superadmin: false,
}

const MockIcon = () => React.createElement('svg')
const renderSidebar = (overrides = {}) => renderToStaticMarkup(React.createElement(WorkspaceSidebar, {
  menu: false,
  collapsed: false,
  section: 'chat',
  settingsArea: false,
  personalTaskItems: [],
  allTaskCount: 0,
  superadmin: false,
  user: undefined,
  t: zh,
  taskReasonIcons: { approval_required: MockIcon, acceptance_required: MockIcon },
  taskStatusIcons: { active: MockIcon, waiting_user: MockIcon, interrupted: MockIcon, paused: MockIcon, completed: MockIcon, cancelled: MockIcon },
  onToggleCollapsed: noop,
  onReturnToChat: noop,
  onNavigate: noop,
  onOpenTask: noop,
  ...overrides,
}))

test('renders brand, primary navigation and omits current tasks section when empty', () => {
  const html = renderSidebar()
  assert.match(html, /ClusBot/)
  assert.match(html, /个人运维助手/)
  assert.match(html, /个人助手/)
  assert.match(html, /任务清单/)
  assert.doesNotMatch(html, /当前事项/)
  assert.doesNotMatch(html, /账号与安全/)
})

test('renders current tasks and counts when active or waiting tasks exist', () => {
  const html = renderSidebar({
    superadmin: true,
    allTaskCount: 5,
    personalTaskItems: [
      { id: 't1', title: '节点复检授权', status: 'waiting_user', status_reason: 'approval_required', reason: '' },
      { id: 't2', title: 'POD7 阶段报告', status: 'waiting_user', status_reason: 'acceptance_required', reason: '' },
      { id: 't3', title: 'POD7 全套基准 · T42', status: 'active', status_reason: '', reason: '' },
    ],
  })
  assert.match(html, /当前事项 \(3\)/)
  assert.match(html, /<b class="sidebar-count" aria-hidden="true">3<\/b>/)
  assert.match(html, /<b class="sidebar-count" aria-hidden="true">5<\/b>/)
  assert.match(html, /节点复检授权/)
  assert.match(html, /待决定/)
  assert.match(html, /POD7 阶段报告/)
  assert.match(html, /待验收/)
  assert.match(html, /POD7 全套基准 · T42/)
  assert.match(html, /运行中/)
})

test('renders account card with role label only when user is present', () => {
  const adminHtml = renderSidebar({ user: adminUser, superadmin: true })
  assert.match(adminHtml, /aria-label="账号与安全"/)
  assert.match(adminHtml, /林工 · admin/)
  assert.match(adminHtml, /<small>超级管理员<\/small>/)
  assert.doesNotMatch(adminHtml, /账号与安全 · /)

  const memberHtml = renderSidebar({ user: memberUser, superadmin: false })
  assert.match(memberHtml, /小李 · member/)
  assert.match(memberHtml, /<small>普通用户<\/small>/)
})
