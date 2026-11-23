import assert from 'node:assert/strict'
import {readFile} from 'node:fs/promises'
import test from 'node:test'
import ts from 'typescript'

// Load the component's actual parser without mounting the React view.
const source=(await readFile(new URL('../src/AssistantRecommendation.tsx',import.meta.url),'utf8')).replace(/^import .*\n/gm,'')
const js=ts.transpileModule(source,{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext,jsx:ts.JsxEmit.React}}).outputText
const {parseRecommendation}=await import('data:text/javascript;base64,'+Buffer.from(js).toString('base64'))
const proposal=()=>({proposal_id:'a'.repeat(48),title:'恢复建议',origin:'model',execution:'not_evaluated',expires_at_unix_ms:Date.now()+300000,options:[{id:'b'.repeat(32),label:'保留现场并续跑',recommended:true}]})

test('accepts one to four bounded, text-only suggestions',()=>{
  assert.ok(parseRecommendation(proposal()))
  const value=proposal()
  value.options.push({id:'c'.repeat(32),label:'复检节点',detail:'由用户自行核实'})
  assert.ok(parseRecommendation(value))
  value.options[1].label='<img src=x onerror=alert(1)>'
  assert.equal(parseRecommendation(value).options[1].label,'<img src=x onerror=alert(1)>')
})

test('rejects malformed or action-bearing suggestions',()=>{
  for(const mutate of [
    value=>value.proposal_id='../settings',
    value=>value.origin='system',
    value=>value.execution='confirmed',
    value=>value.expires_at_unix_ms='tomorrow',
    value=>value.options=[],
    value=>value.options.push({...value.options[0]}),
    value=>value.options[0].id='../../admin',
    value=>value.options[0].label='x'.repeat(81),
    value=>value.options[0].recommended='yes',
    value=>value.options[0].url='https://example.invalid',
    value=>value.action={url:'https://example.invalid'},
  ]) {
    const value=proposal();mutate(value)
    assert.equal(parseRecommendation(value),undefined)
  }
})
