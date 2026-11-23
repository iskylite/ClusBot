import assert from 'node:assert/strict'
import {readFile} from 'node:fs/promises'
import test from 'node:test'
import ts from 'typescript'

const source=await readFile(new URL('../src/benchmarkRevisionInput.ts',import.meta.url),'utf8')
const js=ts.transpileModule(source,{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext}}).outputText
const {isBenchmarkRevisionInstruction,namedBenchmarkTasks}=await import('data:text/javascript;base64,'+Buffer.from(js).toString('base64'))

test('recognizes affirmative plan changes and leaves questions, quotations and other controls alone',()=>{
  assert.equal(isBenchmarkRevisionInstruction('查 abcdef12 四层状态，把未派发的集合通信测试移除，先预览影响。'),true)
  assert.equal(isBenchmarkRevisionInstruction('移除测试项 collective'),true)
  assert.equal(isBenchmarkRevisionInstruction('把本次尚未派发的单机带宽改为 90 秒'),true)
  for(const text of ['集合通信测试能移除吗？','不要移除集合通信测试','如果移除集合通信测试','请介绍移除集合通信测试的影响','日志：移除测试项 collective','暂停任务并移除测试项 collective','查任务进度','/benchmark plan@1'])
    assert.equal(isBenchmarkRevisionInstruction(text),false,text)
})

test('matches only complete IDs or an unambiguous displayed prefix',()=>{
  const one='abcdef12000000000000000000000000', two='abcdef12ffffffffffffffffffffffff', other='12345678000000000000000000000000'
  assert.deepEqual(namedBenchmarkTasks('查 '+one+'，移除集合通信测试',[one,two,other]),[one])
  assert.deepEqual(namedBenchmarkTasks('查 abcdef12，移除集合通信测试',[one,two,other]),[one,two])
  assert.deepEqual(namedBenchmarkTasks('查 abcdef1，移除集合通信测试',[one,two,other]),[])
  assert.deepEqual(namedBenchmarkTasks('查 abcdef12aa，移除集合通信测试',[one,two,other]),[])
})
