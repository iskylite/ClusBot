import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import test from 'node:test'
import React from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import ts from 'typescript'

// Render the real page without its browser-only stylesheet and image imports.
const source = (await readFile(new URL('../src/AccountUI.tsx', import.meta.url), 'utf8'))
  .replace(/^import clusbotIcon .*\n/m, "const clusbotIcon = ''\n")
  .replace(/^import \{ displayDate \} .*\n/m, "const displayDate = () => ''\n")
  .replace(/^import \{ FeedbackCallout \} .*\n/m, "const FeedbackCallout = ({ message }: { message: string }) => React.createElement('p', null, message)\n")
  .replace(/^import '\.\/account-ui\.css'\n/m, '')
const compiled = ts.transpileModule(source, { compilerOptions: {
  target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext, jsx: ts.JsxEmit.ReactJSX,
} }).outputText
const js = compiled.replace(/from ['"](react(?:\/jsx-runtime)?|lucide-react)['"]/g,
  (_, specifier) => `from '${import.meta.resolve(specifier)}'`)
const { AccountPage } = await import('data:text/javascript;base64,' + Buffer.from(js).toString('base64'))

const admin = { principal_id: 'admin', username: 'admin', display_name: '管理员', avatar: '', status: 'active', is_superadmin: true, must_change_password: false }
const member = { ...admin, principal_id: 'member', username: 'member', display_name: '成员', is_superadmin: false }
const render = (account, initialSection) => renderToStaticMarkup(React.createElement(AccountPage, {
  account, initialSection, onAccountChange() {}, onLogout() {},
}))

test('account navigation shows the selected content and retains profile and password deep links', () => {
  const personal = render(admin)
  assert.match(personal, /href="\/account" aria-current="page"[^>]*>本人账号/)
  assert.match(personal, /href="\/account#users"[^>]*>用户管理/)
  assert.match(personal, /编辑头像与姓名/)
  assert.match(personal, /修改本人密码/)
  assert.doesNotMatch(personal, /全站用户管理/)

  const users = render(admin, 'users')
  assert.match(users, /href="\/account#users" aria-current="page"[^>]*>用户管理/)
  assert.match(users, /全站用户管理/)
  assert.match(users, /创建用户/)
  assert.doesNotMatch(users, /aria-label="本人账号"/)

  assert.match(render(admin, 'profile'), /编辑头像与姓名<\/h2>/)
  assert.match(render(admin, 'password'), /name="current_password"/)
})

test('ordinary and forced-password accounts cannot enter user management', () => {
  for (const account of [member, { ...admin, must_change_password: true }]) {
    const html = render(account, 'users')
    assert.match(html, /href="\/account" aria-current="page"[^>]*>本人账号/)
    assert.doesNotMatch(html, /href="\/account#users"/)
    assert.doesNotMatch(html, /全站用户管理/)
  }
})
