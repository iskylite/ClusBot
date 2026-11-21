package httpapi

func assistantDomainTools() []assistantTool {
	return []assistantTool{
		{Name: "get_cluster_trends", Description: "读取已授权节点的历史在役率及故障趋势。query 可传 range（24h、7d 或 30d）。", Method: "GET", Path: "/api/v1/domain/cluster-trends"},
		{Name: "list_benchmark_profiles", Description: "列出已发布基准方案。", Method: "GET", Path: "/api/v1/domain/benchmark-profiles"},
		{Name: "get_benchmark_profile", Description: "查看已发布基准方案的指定版本。参数 key、version。", Method: "GET", Path: "/api/v1/domain/benchmark-profiles/{key}/versions/{version}"},
		{Name: "submit_benchmark", Description: "按已发布方案提交基准。body 包含 request_key、profile、version。正式派发由原权限和预检规则决定。", Method: "POST", Path: "/api/v1/domain/benchmarks", Confirm: true},
		{Name: "get_benchmark_progress", Description: "查看基准执行进展，参数 id 为任务 ID。", Method: "GET", Path: "/api/v1/domain/benchmarks/{id}"},
		{Name: "get_benchmark_report", Description: "查看基准报告，参数 id 为任务 ID。", Method: "GET", Path: "/api/v1/domain/benchmarks/{id}/report"},
		{Name: "list_discovery_candidates", Description: "查阅已有资产发现候选，不启动发现。", Method: "GET", Path: "/api/v1/domain/discovery/candidates"},
		{Name: "get_discovery_observations", Description: "读取指定发现计划的已有观测。参数 id 为计划 ID。", Method: "GET", Path: "/api/v1/domain/discovery/plans/{id}/observations"},
		{Name: "publish_discovery_plan", Description: "使用已获授权的来源发布周期发现计划。body 包含 id、source、scope、period_seconds、first_at_unix_ms。", Method: "POST", Path: "/api/v1/domain/discovery/plans", Confirm: true},
		{Name: "stop_discovery_plan", Description: "停止已有发现计划。参数 id 为计划 ID，body 为 {}。", Method: "DELETE", Path: "/api/v1/domain/discovery/plans/{id}", Confirm: true},
		{Name: "request_ticket", Description: "提交工单请求。请提供 client_request_id、object_ref、fault_evidence。", Method: "POST", Path: "/api/v1/domain/tickets", Confirm: true},
		{Name: "get_ticket_progress", Description: "查看工单请求进展。参数 id 为工单操作 ID。", Method: "GET", Path: "/api/v1/domain/ticket-operations/{id}"},
		{Name: "get_incident_workflow", Description: "查看故障事件的五步处置流程与各阶段执行动作。参数 id 为事件 ID。", Method: "GET", Path: "/api/v1/incidents/{id}/workflow"},
		{Name: "get_incident_step_logs", Description: "展开查看故障处置某一阶段的执行日志输出。参数 id 为事件 ID，step 为阶段标识。", Method: "GET", Path: "/api/v1/incidents/{id}/steps/{step}/logs"},
	}
}
