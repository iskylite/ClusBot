import {Clock3} from 'lucide-react'

const expiryTime=new Intl.DateTimeFormat('zh-CN',{month:'numeric',day:'numeric',hour:'2-digit',minute:'2-digit',hourCycle:'h23'})

export function ApprovalExpiry({at}:{at:number}) {
  return <p className="approval-expiry"><Clock3 size={14} aria-hidden="true"/>
    <span>有效至 <time dateTime={new Date(at).toISOString()}>{expiryTime.format(at)}</time></span>
  </p>
}
