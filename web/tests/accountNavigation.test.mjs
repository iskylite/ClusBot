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
const { AccountPage, decodeAccountSession } = await import('data:text/javascript;base64,' + Buffer.from(js).toString('base64'))

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

test('the workspace needs a verified account, not an anonymous authenticated flag', () => {
  assert.deepEqual(decodeAccountSession({ authenticated: false }), { authenticated: false })
  assert.deepEqual(decodeAccountSession({ authenticated: true, user: member }), { authenticated: true, user: member })
  const forced = { ...admin, must_change_password: true }
  assert.deepEqual(decodeAccountSession({ authenticated: true, user: forced }), { authenticated: true, user: forced })
  for (const malformed of [
    null, {}, { authenticated: 'true' }, { authenticated: true }, { authenticated: true, mode: 'local' },
    { authenticated: true, user: { ...member, principal_id: '' } },
    { authenticated: true, user: { ...member, status: 'disabled' } },
    { authenticated: true, user: { ...member, is_superadmin: 'false' } },
  ]) assert.throws(() => decodeAccountSession(malformed), /无法核对账号会话/)
})

test('a new composer or memory draft is restored only for its account', async () => {
  const source = await readFile(new URL('../src/draftStorage.ts', import.meta.url), 'utf8')
  const js = ts.transpileModule(source, { compilerOptions: {
    target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext,
  } }).outputText
  const { accountDraftKey, accountDraftPrefix, readDraft, writeDraft } = await import('data:text/javascript;base64,' + Buffer.from(js).toString('base64'))
  const key = (area, principal, resource) => accountDraftPrefix + area + '.' + accountDraftKey(principal, resource)
  for (const [area, resource] of [['composer', 'chat:new'], ['composer', 'isolated:new'], ['admission', 'chat:new'], ['memory', 'new/']]) {
    assert.notEqual(key(area, 'user:alice', resource), key(area, 'user:bob', resource))
    const bridge = []
    const entries = new Map()
    try {
      for (const native of [false,true]) {
        globalThis.sessionStorage = { getItem: name => entries.get(name), setItem: (name, value) => entries.set(name, value) }
        globalThis.window = native ? { clusbotDrafts: {'clusbot.composer.v1.chat:new':'unattributed'}, webkit: { messageHandlers: { drafts: { postMessage: event => bridge.push(event) } } } } : {}
        const alice = key(area, 'user:alice', resource)
        const bob = key(area, 'user:bob', resource)
        assert.equal(readDraft(alice), undefined)
        writeDraft(alice, 'first account draft')
        assert.equal(readDraft(bob), undefined)
        assert.equal(readDraft(alice), 'first account draft')
        if (native) assert.deepEqual(bridge.at(-1), { key: alice, value: 'first account draft' })
      }
    } finally {
      delete globalThis.window
      delete globalThis.sessionStorage
    }
  }
})
