// Only route an affirmative instruction about one plan change. Questions,
// examples and other controls continue through ordinary conversation.
export function isBenchmarkRevisionInstruction(value:string) {
  const text=value.trim().toLowerCase()
  if(!text||text.length>4096||/^[\/[>\]$]/.test(text)||/[\r\n?？`'"“”「」]/.test(text))return false
  if(/日志|示例|是否|能否|可否|能不能|可以吗|怎么|如何|假设|假如|如果|建议|考虑|可能|说明|解释|介绍|举例|讨论|不要|别改|先不改|不需要改|不用修改|不修改|不要预演|别预演|what if|how to|do not|暂停|取消|恢复|跳过|重跑|创建任务|新建任务/.test(text))return false
  if(!/(方案|测项|测试项|测试|参数|集合通信|单机带宽)/.test(text))return false
  const opening='(?:^|[，,；;]\\s*)(?:请)?(?:帮我|我要|我想)?'
  const objectFirst=new RegExp(opening+'(?:把|将)[^，,；;]{1,180}(?:移除|删除|去掉|改为|修改为|调整为|更改为)')
  const actionFirst=new RegExp(opening+'(?:移除|删除|去掉|修订|修改|调整|更改)(?:方案|测项|测试项|测试|参数|基准方案|\\s+[a-z0-9_.-]+)')
  return objectFirst.test(text)||actionFirst.test(text)
}

// The UI displays the first eight characters in its benchmark card. Match
// only a whole displayed prefix or the complete ID against known task IDs.
export function benchmarkTaskTokens(text:string) {
  return [...new Set([...text.matchAll(/(?:^|[^a-z0-9])([a-f0-9]{8}|[a-f0-9]{32})(?=$|[^a-z0-9])/gi)].map(match=>match[1].toLowerCase()))]
}

export function namedBenchmarkTasks(text:string,taskIDs:string[]) {
  const tokens=benchmarkTaskTokens(text)
  return [...new Set(taskIDs.filter(id=>tokens.some(token=>id.toLowerCase().startsWith(token))))]
}
