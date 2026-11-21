package httpapi

// assistantTaskTools exposes existing business operations through their normal
// HTTP handlers. Those handlers remain responsible for account isolation,
// revisions and domain-specific state checks; every mutation requires a
// separate confirmation in the assistant gateway.
func assistantTaskTools() []assistantTool {
	return []assistantTool{
		{Name: "list_tasks", Description: "搜索本人的任务。query 可传 q、status、scope_label、limit、after；状态为 active、waiting_user、interrupted、completed 或 cancelled。", Method: "GET", Path: "/api/v1/tasks"},
		{Name: "get_task", Description: "读取本人任务详情、当前进展和成果；params.id 为任务 ID。", Method: "GET", Path: "/api/v1/tasks/{id}"},
		{Name: "list_task_runs", Description: "读取本人任务的执行记录；params.id 为任务 ID，query.after 为分页游标。", Method: "GET", Path: "/api/v1/tasks/{id}/runs"},
		{Name: "get_task_run", Description: "读取本人任务的一次执行详情；params.id 为任务 ID，params.run_id 为执行 ID。", Method: "GET", Path: "/api/v1/tasks/{id}/runs/{run_id}"},
		{Name: "create_task", Description: "创建并提交本人任务。body 需包含 client_request_id、text；可选 scope_label、acceptance_criteria、required_filename 和 attachment_ids。", Method: "POST", Path: "/api/v1/tasks", Confirm: true},
		{Name: "edit_task", Description: "修改本人任务。params.id 为任务 ID；body 需包含当前 revision 及要修改的 title、goal、scope_label、acceptance_criteria 或 required_filename。", Method: "PATCH", Path: "/api/v1/tasks/{id}", Confirm: true},
		{Name: "pause_task", Description: "暂停本人任务，阻止后续派发。params.id 为任务 ID；body 包含当前 revision。", Method: "POST", Path: "/api/v1/tasks/{id}/pause", Confirm: true},
		{Name: "cancel_task", Description: "取消本人任务。params.id 为任务 ID；body 包含当前 revision。取消仍受已有状态和外部结果核对约束。", Method: "POST", Path: "/api/v1/tasks/{id}/cancel", Confirm: true},
		{Name: "resume_task", Description: "继续、重试或重新分配本人任务。params.id 为任务 ID；body 需包含当前 revision、client_request_id，可选 retry 或 reassign。", Method: "POST", Path: "/api/v1/tasks/{id}/resume", Confirm: true},
		{Name: "accept_task", Description: "验收本人任务的当前成果。params.id 为任务 ID；body 包含 artifact_id 和当前 revision。", Method: "POST", Path: "/api/v1/tasks/{id}/acceptance", Confirm: true},
		{Name: "acknowledge_task_read", Description: "确认本人任务的读取结果未知并按未取得处理，不代表读取成功。params.id 为任务 ID；body 包含 run_id 和当前 revision。", Method: "POST", Path: "/api/v1/tasks/{id}/ack-read", Confirm: true},
		{Name: "list_artifacts", Description: "列出本人成果；query 可传 task_id、scope_label、after。", Method: "GET", Path: "/api/v1/artifacts"},
		{Name: "get_artifact", Description: "读取本人成果详情和正文；params.id 为成果 ID。", Method: "GET", Path: "/api/v1/artifacts/{id}"},
		{Name: "get_artifact_content", Description: "读取本人成果正文；params.id 为成果 ID。", Method: "GET", Path: "/api/v1/artifacts/{id}/content"},
		{Name: "request_task_publication", Description: "申请将本人成果保存到工作区，尚不会完成写入。params.id 为任务 ID；body 包含 artifact_id、filename、client_request_id、当前 revision。", Method: "POST", Path: "/api/v1/tasks/{id}/publications", Confirm: true},
		{Name: "decide_publication", Description: "批准或拒绝工作区写入；批准可能实际写入文件。params.id 为写入申请 ID；body 包含 decision（approve 或 deny）及当前 revision。", Method: "POST", Path: "/api/v1/publications/{id}/decision", Confirm: true},
		{Name: "reconcile_publication", Description: "核对本人工作区写入申请的实际状态。params.id 为申请 ID；body 为 {}。", Method: "POST", Path: "/api/v1/publications/{id}/reconcile", Confirm: true},

		{Name: "admin_list_tasks", Description: "超级管理员搜索全站任务；query 可传 scope（all/mine/others）、owner、q、status、scope_label、limit、after。结果包含 owner_id 和任务摘要，不包含私人对话。", Method: "GET", Path: "/api/v1/admin/tasks"},
		{Name: "admin_list_task_notices", Description: "超级管理员读取其他成员任务的精简只读动态通知；query 可传 scope（others/all）、limit，不含操作按钮与私人对话。", Method: "GET", Path: "/api/v1/admin/task-notices"},
		{Name: "admin_get_task", Description: "超级管理员读取跨账号任务摘要；params.owner 为 owner_id，params.id 为任务 ID，不返回私人对话。", Method: "GET", Path: "/api/v1/admin/tasks/{owner}/{id}"},
		{Name: "admin_list_task_runs", Description: "超级管理员读取跨账号任务的执行状态；params.owner 为 owner_id，params.id 为任务 ID，query.after 可分页。", Method: "GET", Path: "/api/v1/admin/tasks/{owner}/{id}/runs"},
		{Name: "admin_list_task_artifacts", Description: "超级管理员列出跨账号任务成果；params.owner 为 owner_id，params.id 为任务 ID，query.after 可分页。", Method: "GET", Path: "/api/v1/admin/tasks/{owner}/{id}/artifacts"},
		{Name: "admin_get_task_artifact_content", Description: "超级管理员读取跨账号任务的成果正文；params.owner 为 owner_id，params.id 为任务 ID，params.artifact 为成果 ID。", Method: "GET", Path: "/api/v1/admin/tasks/{owner}/{id}/artifacts/{artifact}/content"},
		{Name: "admin_edit_task", Description: "超级管理员修改任务标题。params.owner 为 owner_id，params.id 为任务 ID；body 包含当前 revision 和 title。其他账号的任务目标、范围和交付要求仍由原账号修改。", Method: "PATCH", Path: "/api/v1/admin/tasks/{owner}/{id}", Confirm: true},
		{Name: "admin_pause_task", Description: "超级管理员暂停任务。params.owner 为 owner_id，params.id 为任务 ID；body 包含当前 revision。", Method: "POST", Path: "/api/v1/admin/tasks/{owner}/{id}/pause", Confirm: true},
		{Name: "admin_cancel_task", Description: "超级管理员取消任务。params.owner 为 owner_id，params.id 为任务 ID；body 包含当前 revision。", Method: "POST", Path: "/api/v1/admin/tasks/{owner}/{id}/cancel", Confirm: true},
		{Name: "admin_resume_task", Description: "超级管理员继续、重试或重新分配任务。params.owner 为 owner_id，params.id 为任务 ID；body 包含当前 revision、client_request_id，可选 retry 或 reassign。", Method: "POST", Path: "/api/v1/admin/tasks/{owner}/{id}/resume", Confirm: true},
		{Name: "admin_accept_task", Description: "超级管理员验收任务成果。params.owner 为 owner_id，params.id 为任务 ID；body 包含 artifact_id 和当前 revision。", Method: "POST", Path: "/api/v1/admin/tasks/{owner}/{id}/acceptance", Confirm: true},

		{Name: "get_task_workflow", Description: "查看本人任务的多步骤计划和子任务状态；params.id 为父任务 ID。", Method: "GET", Path: "/api/v1/tasks/{id}/workflow"},
		{Name: "create_workflow", Description: "创建本人多步骤计划并提交执行。body 包含 client_request_id、goal、scope_label、acceptance_criteria、steps；每步包含 key、goal、acceptance_criteria，可选 depends_on、optional。", Method: "POST", Path: "/api/v1/workflows", Confirm: true},
		{Name: "list_goals", Description: "列出本人的周期目标及其计划状态。", Method: "GET", Path: "/api/v1/goals"},
		{Name: "create_goal", Description: "创建本人的周期目标。body 包含 client_request_id、target、interval_ms、next_at、expires_at、remaining，可选 scope_label、acceptance_criteria；时间为 Unix 毫秒。", Method: "POST", Path: "/api/v1/goals", Confirm: true},
		{Name: "stop_goal", Description: "停止本人周期目标，同时停止其活动任务。params.id 为目标 ID；body 包含当前 revision。", Method: "POST", Path: "/api/v1/goals/{id}/stop", Confirm: true},

		{Name: "list_attention", Description: "查看当前账号的待处理事项快照；query 可传 limit、before_at 和 before_id，后两项须同时提供。", Method: "GET", Path: "/api/v1/attention"},
		{Name: "get_attention", Description: "读取当前账号一项待处理事项的最新状态和来源；params.id 为待处理事项 ID。", Method: "GET", Path: "/api/v1/attention/{id}"},
		{Name: "list_decisions", Description: "查看当前账号的待决定审批及工作区写入申请。", Method: "GET", Path: "/api/v1/decisions"},
		{Name: "decide_approval", Description: "处理当前账号的领域审批；批准可能允许原操作继续。params.id 为审批 ID；body 包含 decision（approved 或 rejected）。", Method: "POST", Path: "/api/v1/approvals/{id}/decision", Confirm: true},

		{Name: "list_assets", Description: "读取当前工作区的资产台账快照，包括资产状态。", Method: "GET", Path: "/api/v1/domain/snapshot"},
		{Name: "query_asset_acceptance", Description: "创建当前工作区的资产验收状态查询任务。body 包含 client_request_id 和 object_ref，或包含明确查询意图的 text。", Method: "POST", Path: "/api/v1/domain/asset-acceptance", Confirm: true},
		{Name: "get_asset_acceptance", Description: "读取当前工作区的资产验收状态查询任务进度和结果；params.task_id 为查询任务 ID。", Method: "GET", Path: "/api/v1/domain/asset-acceptance/{task_id}"},
		{Name: "lend_asset", Description: "在本机模式登记并确认资产借出。body 包含 object_ref、lender、borrower、due_at（Unix 毫秒），可选 read_only_scope。多人服务器模式暂不提供。", Method: "POST", Path: "/api/v1/loans", Confirm: true},
		{Name: "return_asset", Description: "在本机模式登记资产归还。body 包含 object_ref 和核对项 identity_verified、current_state_known、repair_completed、operations_reconciled、responsibility_transferred；核对事实须由用户提供。多人服务器模式暂不提供。", Method: "POST", Path: "/api/v1/loans/return", Confirm: true},
	}
}
