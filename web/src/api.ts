export type Task = {
  id: string; conversation_id: string; title: string; goal: string; scope_label: string
  acceptance_criteria?: string
  required_filename?: string
  requirements_changed?: boolean
  status: 'active' | 'waiting_user' | 'interrupted' | 'completed' | 'cancelled'
  reason: string; status_reason: string; state_changed_at: number
  revision: number; updated_at: number
}
export type MessageTaskRef = {task_id:string}
export type DeliveryUpdate = {task_ref?:MessageTaskRef;benchmark_revision_target?:{task_id:string;expected_revision:number};acceptance_criteria?:string;required_filename?:string;revision?:number;mcp_read?:TaskReadRequest;mcp_reads?:TaskReadRequest[];replace_materials?:boolean}
export type Acceptance = {artifact_id:string;sha256:string;criteria:string;accepted_at:number;publication_id?:string;filename?:string}
export type ExecutionUsage = {
  model_requests:number; unreported_requests:number; active_ms:number; time_partial:boolean; untracked_runs:number
  input_tokens:number|null; output_tokens:number|null; last_input_tokens?:number|null
}
export type BenchmarkExecutionSuggestion={task_id:string;step_id:string;case_key:string;plan_revision:number;state:string}
export type BenchmarkExecutionReceipt={task_id:string;step_id:string;plan_revision:number;action:string;state:string}
export type Run = { id: string; task_id?: string; status: string; response: string; error?: string; error_code?: string; usage?: ExecutionUsage; execution_generation: number; benchmark_suggestion?:{target:string;profile:{profile:string;version:number};state:string}; benchmark_revision_preview?:{task_id:string;source_run_id?:string;expected_revision:number;applied_revision?:number}; benchmark_revision_suggestion?:{task_id:string;expected_revision:number}; benchmark_execution_suggestion?:BenchmarkExecutionSuggestion; benchmark_execution_control?:BenchmarkExecutionReceipt }
export type RunListItem = {id:string; status:string; created_at:number; input_excerpt?:string}
export type RunPage = {items:RunListItem[]; next_cursor?:string}
export type RunModelContext = {execution_generation:number;recorded_at:number;manifest?:{requested_model?:string};
  model_receipt?:{protocol:string;state:'reported'|'not_reported'|'conflicting';reported_model?:string}}
export type RunRecord = Run & {
  created_at:number; event_after:number; messages:Message[]; inherited_input?:Message
  reads?:TaskRead[]; artifacts?:Artifact[]; publications?:Publication[]; contexts?:RunModelContext[]
}
export type Conversation = { id: string; title: string; archived:boolean; revision:number }
export type ConversationPage = {items:Conversation[];next_cursor?:string}
export type MessagePage = {items:Message[];before_cursor?:number;after_cursor?:number}
export type Artifact = { id: string; task_id: string; name: string; content?: string; created_at: number; run_id?:string; task_title?:string; scope_label?:string; accepted_at?:number; sha256?:string; acceptance?:Acceptance }
export type Publication = {
  id: string; artifact_id: string; filename: string; status: string; detail?: string; expires_at: number
  previous_sha256?: string; previous_content?: string
}
export const awaitingPublication = (publication?: Publication) =>
  publication !== undefined && ['pending','dispatched','unknown'].includes(publication.status)
export type UploadPart = {id:string;name:string;sha256:string;bytes:number;part:number;parts:number}
export type Material = {id:string;path:string;sha256:string;size:number;captured_at:number;upload?:UploadPart}
export type Message = {id?:number; run_id?:string; role:'user'|'assistant'; text:string; asset_query_task_id?:string;mcp_onboarding?:boolean; assistant_task_list?:boolean; assistant_asset_list?:boolean; assistant_attention_list?:boolean; assistant_artifact_list?:boolean; assistant_operation_advice?:boolean; benchmark_admission?:boolean; assistant_skill_options?:boolean; assistant_benchmark_comparison?:boolean; assistant_discovery_schedule?:boolean; has_model_receipt?:boolean; task_ref?:MessageTaskRef & {title?:string}; materials?:Material[];replace_materials?:boolean;memory_refs?:MemoryRef[];incomplete?:boolean}
export type MaterialFile = {path:string;size:number}
export type SkillReplayMetrics = {
  cases:number; completed:number; incomplete?:number; successful:number; success_rate:number
  wrong_reuse_checked:number; wrong_reuse_count:number
  input_tokens_known_total:number; input_usage_reported:number
  output_tokens_known_total:number; output_usage_reported:number; duration_ms:number
}
export type SkillReplayArm = 'none'|'base_skill'|'proposed_skill'|'current'|'candidate'
export type SkillReplay = {
  id:string; passed:boolean; contract?:string; suite_sha256?:string; model_config:{model:string}
  gains?:string[]; regressions?:string[]; incomplete?:string[]
  metrics:Partial<Record<SkillReplayArm,SkillReplayMetrics>>
  results:{case_id:string;policy:SkillReplayArm;status:string;success:boolean;wrong_reuse:boolean|null}[]
}
export type SkillProposal = {
  id:string; goal:string; base_version:number|null; base_instructions?:string; base_sha256?:string
  suite_sha256?:string; evaluation_run_id?:string; match_mode?:'goal_exact'|'scope_keywords'|'scope_all'
  match_keywords?:string[]; draft_mode?:'template'|'synthesized'
  status:'pending'|'active'|'historical'|'stale'|'unavailable'
  skill:{skill_id:string;scope_label:string;version:number;revision:number;sha256:string;instructions:string;active:boolean}
  evidence:{gains?:string[];regressions?:string[];paired_cases:number;incomplete_pairs:number;current_successful:number;candidate_successful:number
    reuse_checked:number;current_wrong_reuse:number;candidate_wrong_reuse:number;token_pairs:number;candidate_token_delta:number}
  sources:{candidate_id:string;task_id?:string;artifact_id?:string;message_id?:number}[]
  replay?:SkillReplay
}
export type ExperienceEvaluationReport = {
  version:number; suite_sha256?:string; snapshot_sha256:string; scoring:string
  model_config:{model:string}
  metrics:Record<string,SkillReplayMetrics>
  case_contexts:{id:string;scope_label:string;goal:string}[]
  results:{case_id:string;policy:string;status:string;success:boolean;wrong_reuse:boolean|null}[]
}
export type ExperienceEvaluationRun = {
  id:string; suite_id:string; scope_label:string; goal:string; suite_sha256:string
  report:ExperienceEvaluationReport; created_at:number
}
export type ExperienceCandidate = {
  id:string; task_id:string; kind:'accepted_result'|'user_correction'; scope_label:string
  goal?:string; acceptance_criteria?:string; format_cues?:string[]; correction?:string; correction_truncated?:boolean
  artifact_id?:string; message_id?:number; counter_artifact_id?:string
  status:'pending'|'approved'|'rejected'|'unavailable'; revision:number
}
export type MemoryKind = 'preference' | 'fact'
export type MemoryRef = {id:string;revision:number;title?:string;current_revision:number}
export type MemorySource = {message_id:number;path:string;title:string;text?:string}
export type Memory = {id:string;kind:MemoryKind;title:string;content:string;pinned:boolean;revision:number;updated_at:number;source?:MemorySource;source_deleted?:boolean}
export type MCPConfig = {transport:'stdio'|'http';endpoint?:string;token_env?:string;command?:string;args?:string[];env?:string[]}
export type MCPTool = {name:string;title?:string;description?:string;input_schema:unknown;output_schema?:unknown;schema_error?:string;version:string;allowed:boolean;read_only?:boolean}
export type MCPConnection = {id:string;name:string;config:MCPConfig;enabled:boolean;revision:number;status:'untested'|'ready'|'failed';detail?:string;protocol?:string;checked_at?:number;tool_count:number;tools?:MCPTool[]}
export type MCPResultReference = {id:string;bytes:number;sha256:string}
export type CapabilityBinding = {
  capability_id:string; capability_key:string; version:number; kind:'query'|'action'; target_kind?:string;
  execution_mode:'sync'|'async'|'deterministic'; connection_id:string; connection_name:string; connection_revision:number;
  start_tool:string; lookup_tool?:string; get_tool?:string; cancel_tool?:string;
  idempotent?:string; supports_get?:string; supports_cancel?:string; verified_scope?:string; exposed_to_model:boolean; updated_at:number
}
export type CapabilityBindingInput = {
  capability_key:string; version?:number; kind:'query'|'action'; target_kind?:string;
  execution_mode?:'sync'|'async'|'deterministic'; connection_id:string;
  start_tool:string; lookup_tool?:string; get_tool?:string; cancel_tool?:string;
  idempotent?:string; supports_get?:string; supports_cancel?:string; verified_scope?:string; exposed_to_model?:boolean
}
export type MCPOutput = {text?:string;structured?:unknown;is_error:boolean;truncated:boolean;media_omitted:boolean;reference?:MCPResultReference;validation_error?:string}
export type MCPResult = MCPOutput & {id:string;received_at:number}
export type TaskReadRequest = {connection_id:string;revision:number;tool_name:string;tool_version:string;arguments:Record<string,unknown>}
export type TaskToolOption = {connection_id:string;connection_name:string;revision:number;tool:MCPTool}
export type TaskRead = {position:number;connection_name:string;tool_name:string;tool_title?:string;status:string;detail?:string;received_at?:number;result?:MCPOutput;acknowledged?:boolean;source_run_id?:string}
export type Detail = {
  acceptance?: Acceptance
  task_usage?: ExecutionUsage
  read?: TaskRead
  reads?: TaskRead[]
  read_checkpoint?: 'ready'|'retry'|'unavailable'
  unknown_read_run_id?: string
  workflow?: {parent_id: string; children: {task_id: string; title: string; reason: string}[]}
  workflow_parent_id?: string
  task?: Task; conversation: Conversation; run?: Run; artifact?: Artifact; publication?: Publication
  artifact_versions?: Artifact[]
  memory_refs?: MemoryRef[]
  messages: Message[]
}
export type OverviewGroup = 'waiting' | 'active' | 'completed'
export type Overview = {
  pending_skill_proposals?: number
  scopes:string[]; groups:Record<OverviewGroup,Task[]>; snapshot_at:number
  counts:Record<OverviewGroup,number>; next_cursors:Partial<Record<OverviewGroup,string>>
}
export type ModelProtocol = 'chat_completions' | 'responses'
export type ModelAttributes = { context_tokens?: number; max_output_tokens?: number; vision?: boolean; file_input?:boolean; tool_calling?:boolean; structured_output?:boolean; reasoning?: boolean; reasoning_levels?: string[]; reasoning_effort?: string; sources?:Partial<Record<'context_tokens'|'max_output_tokens'|'vision'|'file_input'|'tool_calling'|'structured_output'|'reasoning'|'reasoning_levels'|'reasoning_effort','provider'|'manual'>> }
export type DiscoveredModel = { id: string; name?:string; attributes: ModelAttributes }
export type ModelSettings = { attributes?: ModelAttributes; protocol: ModelProtocol; base_url: string; model_id: string; api_key_ref: string; revision: number }
export type Settings = ModelSettings & {
  mode: 'demo' | ModelProtocol | 'unconfigured'
  base_url_source?: string
  model_id_source?: string
  key_configured: boolean
  local_key_saved: boolean
  effective_context_tokens?: number
  model_concurrency?: number
  workspace_path: string
}
export type Receipt = {
  task_id?: string; conversation_id?: string; run_id?: string
  benchmark_revision_suggestion?:{task_id:string;expected_revision:number}
  model_pending?: boolean; state?: string; reason?: string
}

export class APIError extends Error {
  constructor(message:string,readonly code?:string,readonly status?:number) {super(message);this.name='APIError'}
}

export async function api<T>(path: string, data?: unknown, method = data === undefined ? 'GET' : 'POST', extraHeaders:Record<string,string> = {}): Promise<T> {
  const form=data instanceof FormData
  const response = await fetch('/api/v1' + path, {
    method,
    headers: { ...(data === undefined || form ? {} : { 'Content-Type': 'application/json' }), ...extraHeaders },
    body: data === undefined ? undefined : form ? data : JSON.stringify(data),
  })
  if (!response.ok) {
    const body = await response.json().catch(() => null)
    if (response.status === 401 && path !== '/session') window.dispatchEvent(new Event('clusbot:logout'))
    throw new APIError(body?.message || body?.error || '请求未完成，请稍后重试',body?.code,response.status)
  }
  return response.status === 204 ? undefined as T : response.json()
}

export const activeRun = (run?: Run) => run?.status === 'queued' || run?.status === 'running'
