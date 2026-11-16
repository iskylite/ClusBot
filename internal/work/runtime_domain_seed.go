package work

// V3 领域台账演示种子数据（仅在 -demo 模式注入）。
//
// 设计约束：
//   - 幂等：只在 ops_assets 为空时插入；所有写用 INSERT OR IGNORE；
//   - 所有 NOT NULL 列都显式给值，CHECK 约束的状态值严格取自 runtime_*.sql；
//   - 时间一律毫秒整数（time.Now().UnixMilli() 为基准）；
//   - JSON 字符串列写成合法 JSON 文本但保持 string，不解析成对象；
//   - 有外键的表按父表在前的顺序插入（foreign_keys=ON）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// domainSeedTable 描述一张演示表：列顺序与 rows 的列顺序一一对应。
type domainSeedTable struct {
	table string
	cols  []string
	rows  [][]any
}

// seedDomainInsert 用参数化 INSERT OR IGNORE 批量写入，避免拼接字符串字面量。
func seedDomainInsert(ctx context.Context, tx *sql.Tx, t domainSeedTable) error {
	if len(t.rows) == 0 {
		return nil
	}
	marks := make([]string, len(t.cols))
	for i := range marks {
		marks[i] = "?"
	}
	stmt := fmt.Sprintf("INSERT OR IGNORE INTO %s (%s) VALUES (%s)",
		t.table, strings.Join(t.cols, ","), strings.Join(marks, ","))
	prepared, err := tx.PrepareContext(ctx, stmt)
	if err != nil {
		return fmt.Errorf("领域演示数据 %s 预处理失败：%w", t.table, err)
	}
	defer prepared.Close()
	for i, row := range t.rows {
		if len(row) != len(t.cols) {
			return fmt.Errorf("领域演示数据 %s 第 %d 行列数 %d 与列定义 %d 不符", t.table, i+1, len(row), len(t.cols))
		}
		if _, err := prepared.ExecContext(ctx, row...); err != nil {
			return fmt.Errorf("领域演示数据 %s 第 %d 行写入失败：%w", t.table, i+1, err)
		}
	}
	return nil
}

// seedJSON 把演示值序列化成 JSON 文本，作为 string 列写入（契约要求不解析）。
func seedJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// SeedDomainDemo 注入一套贴近前端设计样例的演示数据。
// 幂等：一旦 ops_assets 已有记录就整体跳过，正常启动不会调用本函数。
func (s *Store) SeedDomainDemo(ctx context.Context) error {
	var existing int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM ops_assets").Scan(&existing); err != nil {
		return fmt.Errorf("检查领域演示数据失败：%w", err)
	}
	if existing > 0 {
		return nil
	}

	now := time.Now().UnixMilli()
	day := int64(24 * 60 * 60 * 1000)
	ago := func(days int) int64 { return now - int64(days)*day }

	tables := []domainSeedTable{
		// 0. 演示会话与 Task：让工作台/任务/审批页在 -demo 下也有真实任务可读。
		{table: "conversations", cols: []string{"id", "title", "created_at", "archived", "revision"}, rows: [][]any{
			{"conv-demo-1", "B03 批次写入验收报告", ago(1), 0, 1},
			{"conv-demo-2", "整理本周工作周报", ago(2), 0, 1},
			{"conv-demo-3", "node-041 网卡错包率调查", ago(1), 0, 1},
			{"conv-demo-4", "B02 批次交付报告", ago(3), 0, 1},
			{"conv-chat-1", "现在集群什么情况？", ago(0), 0, 1},
			{"conv-chat-2", "B03 什么时候能交付？", ago(1), 0, 1},
		}},
		{table: "tasks", cols: []string{
			"id", "conversation_id", "title", "goal", "scope_label", "status", "reason", "revision",
			"updated_at", "completed_at", "acceptance_criteria", "required_filename",
			"requirements_changed", "status_reason", "state_changed_at", "priority",
		}, rows: [][]any{
			{"00000000000000000000000000000001", "conv-demo-1", "B03 批次写入验收报告", "把 B03 批次联机测试结果整理成验收报告并写入 outputs/", "B03 批次 · 16 台", "waiting_user", "等待你确认写入", 3, ago(1), nil, "覆盖 16 台节点与 6 项验收条件", "outputs/B03-交付验收报告.md", 0, "等待人工写入授权", ago(1), "high"},
			{"00000000000000000000000000000002", "conv-demo-2", "整理本周工作周报", "把三份会议记录整理成可交付的周报", "工作资料", "active", "正在撰写第 3 版草稿", 2, now, nil, "", "outputs/本周工作周报.md", 0, "撰写中", now, "normal"},
			{"00000000000000000000000000000003", "conv-demo-3", "node-041 网卡错包率调查", "采集近 2 小时观测并比对历史案例，只读不写设备", "node-041", "active", "证据采集中", 1, now, nil, "", "", 0, "证据采集 2 / 4", now, "high"},
			{"00000000000000000000000000000004", "conv-demo-4", "B02 批次交付报告", "核对 8 台节点交付证据并生成报告", "B02 批次 · 8 台", "completed", "验收通过", 2, ago(2), ago(2), "6 / 6 证据齐全", "outputs/B02-交付验收报告.md", 0, "completed", ago(2), "normal"},
		}},
		{table: "runs", cols: []string{"id", "conversation_id", "task_id", "status", "response", "created_at"}, rows: [][]any{
			{"run-demo-1", "conv-demo-1", "00000000000000000000000000000001", "completed", "已整理 B03 验收报告，等待你批准写入。", ago(1)},
			{"run-demo-2", "conv-demo-2", "00000000000000000000000000000002", "running", "", now},
			{"run-demo-3", "conv-demo-3", "00000000000000000000000000000003", "running", "", now},
			{"run-demo-4", "conv-demo-4", "00000000000000000000000000000004", "completed", "B02 交付报告已通过验收。", ago(2)},
			{"run-chat-1", "conv-chat-1", nil, "completed", "128 台里 124 台在役，在役率 96.9%。", now},
			{"run-chat-2", "conv-chat-2", nil, "completed", "B03 卡在联机测试，16 台里 11 台通过，预计 16:20 完成。", ago(1)},
		}},
		{table: "messages", cols: []string{"conversation_id", "run_id", "role", "text", "replace_materials"}, rows: [][]any{
			{"conv-demo-1", "run-demo-1", "user", "B03 批次联调结果帮我整理成验收报告。", 0},
			{"conv-demo-1", "run-demo-1", "assistant", "收到，正在核对 16 台节点证据；报告草稿已生成，等待你批准写入。", 0},
			{"conv-demo-2", "run-demo-2", "user", "整理本周工作周报。", 0},
			{"conv-demo-2", "run-demo-2", "assistant", "已读取 3 份会议记录，正在生成第 3 版草稿。", 0},
			{"conv-demo-3", "run-demo-3", "user", "node-041 网卡错包率为什么升高？", 0},
			{"conv-demo-3", "run-demo-3", "assistant", "正在采集近 2 小时观测，并比对历史案例。", 0},
			{"conv-demo-4", "run-demo-4", "user", "B02 交付报告好了吗？", 0},
			{"conv-demo-4", "run-demo-4", "assistant", "6 / 6 证据齐全，已交付。", 0},
			{"conv-chat-1", "run-chat-1", "user", "现在集群什么情况？", 0},
			{"conv-chat-1", "run-chat-1", "assistant", "128 台里 124 台在役，在役率 96.9%。2 台要看一眼，另有 1 项操作结果没确认。", 0},
			{"conv-chat-2", "run-chat-2", "user", "B03 什么时候能交付？", 0},
			{"conv-chat-2", "run-chat-2", "assistant", "B03 卡在联机测试，16 台里 11 台通过，预计 16:20 完成。", 0},
		}},
		// 0b. 演示成果：给演示任务提供可预览的 Artifact。
		{table: "artifacts", cols: []string{"id", "run_id", "task_id", "name", "content", "sha256", "created_at"}, rows: [][]any{
			{"artifact-demo-1", "run-demo-2", "00000000000000000000000000000002", "本周工作周报.md", "# 本周工作周报\n\n## 一、本周进展\n\n- 完成回归测试（Lena，11/14）\n- 定价页文案定稿（Ari，11/15）\n", "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90", ago(0)},
		}},
		// 1. 资产：3 个待纳管、1 个借出、1 个维护中、1 个在役。
		{table: "ops_assets", cols: []string{
			"object_ref", "kind", "identity", "state", "revision", "ownership_epoch",
			"frozen_at", "frozen_by", "freeze_reason", "current_owner", "created_at", "updated_at",
		}, rows: [][]any{
			{"node-041", "计算节点", "10.42.3.41 / SN-88F2-0114", "在役", 33, 4, 0, "", "", "prod-7", ago(13), now},
			{"node-112", "计算节点", "10.42.3.112 / SN-88F2-0088", "维护中", 12, 9, ago(1), "svc-selfheal", "监测代理恢复中，阻止冲突写", "prod-7", ago(18), ago(1)},
			{"node-g-11", "GPU 节点", "10.43.1.11 / SN-88F2-0301", "借出", 41, 2, 0, "", "", "AI 平台组", ago(87), ago(56)},
			{"node-n-01", "计算节点", "待核对 / 无序列号", "待纳管", 1, 0, 0, "", "身份未核对，禁止写操作", "—", ago(1), ago(1)},
			{"node-n-02", "GPU 节点", "待核对 / 无序列号", "待纳管", 1, 0, 0, "", "身份未核对，禁止写操作", "—", ago(1), ago(1)},
			{"node-n-03", "GPU 节点", "待核对 / 无序列号", "待纳管", 1, 0, 0, "", "身份未核对，禁止写操作", "—", ago(1), ago(1)},
		}},

		// 2. 领域包（父表，必须先于 object_types / relation_types）。
		{table: "domain_packages", cols: []string{
			"package_id", "name", "version", "state", "definitions", "definitions_digest",
			"compatibility", "impact_summary", "rollback_basis", "published_by", "published_at",
			"created_at", "updated_at",
		}, rows: [][]any{
			{"pkg-core-v12", "ops-core", 12, "published", seedJSON(map[string]any{"spec": "v12", "types": []string{"node", "nic", "gpu"}}), "digest-v12", "兼容 v11 历史记录", "新增 NIC 错误率阈值", "回退到 pkg-core-v11", "u-lee", ago(30), ago(30), ago(13)},
			{"pkg-core-v11", "ops-core", 11, "superseded", seedJSON(map[string]any{"spec": "v11"}), "digest-v11", "", "v12 发布前的基线", "", "u-lee", ago(90), ago(90), ago(30)},
		}},

		// 3. 8 条对象类型定义。
		{table: "domain_object_types", cols: []string{
			"package_id", "type_key", "fields", "cardinality", "identity_rule",
			"data_binding", "config_spec", "test_spec", "created_at",
		}, rows: [][]any{
			{"pkg-core-v12", "node", seedJSON([]map[string]any{{"name": "kind", "type": "string"}, {"name": "identity", "type": "string"}}), "{}", "serial", "{}", "{}", "{}", ago(30)},
			{"pkg-core-v12", "nic", seedJSON([]map[string]any{{"name": "speed", "type": "string", "unit": "Gb/s"}}), "{}", "serial", "{}", "{}", "{}", ago(30)},
			{"pkg-core-v12", "gpu", seedJSON([]map[string]any{{"name": "count", "type": "int", "unit": "块"}}), "{}", "serial", "{}", "{}", "{}", ago(30)},
			{"pkg-core-v12", "spec", seedJSON([]map[string]any{{"name": "version", "type": "string"}}), "{}", "name+version", "{}", "{}", "{}", ago(30)},
			{"pkg-core-v11", "node", seedJSON([]map[string]any{{"name": "identity", "type": "string"}}), "{}", "serial", "{}", "{}", "{}", ago(90)},
		}},

		// 4. 7 条关系类型定义。
		{table: "domain_relation_types", cols: []string{
			"package_id", "relation_key", "source_type", "target_type",
			"cardinality", "mutually_exclusive_with", "created_at",
		}, rows: [][]any{
			{"pkg-core-v12", "physical_component", "node", "nic", "{}", "[]", ago(30)},
			{"pkg-core-v12", "physical_component_gpu", "node", "gpu", "{}", "[]", ago(30)},
			{"pkg-core-v12", "applies_to", "spec", "node", "{}", "[]", ago(30)},
		}},

		// 5. 事实（含 authoritative / candidate / conflicting / unknown 四种权威）。
		{table: "facts", cols: []string{
			"fact_id", "object_ref", "field_key", "value", "unit", "source_ref", "authority",
			"valid_from", "valid_to", "recorded_at", "revision", "supersedes",
			"conflict_with", "conflict_basis", "created_at",
		}, rows: [][]any{
			{"fact-001", "node-041", "nic.err.packets", "0.14", "%", "node-exporter", "authoritative", ago(1), 0, ago(1), 1, "", "", "", ago(1)},
			{"fact-002", "node-041", "nic.temperature", "68", "°C", "bmc-sensor", "authoritative", ago(1), 0, ago(1), 1, "", "", "", ago(1)},
			{"fact-003", "node-041", "nic.speed", "25", "Gb/s", "switch-api", "candidate", ago(1), ago(1), ago(1), 1, "", "", "", ago(1)},
			{"fact-004", "node-041", "nic.firmware", "2.18", "", "bmc-inventory", "conflicting", ago(1), 0, ago(1), 2, "fact-003", "fact-003", "固件版本与交换机读数不一致", ago(1)},
			{"fact-005", "node-112", "system.health", "unknown", "", "bmc-sensor", "unknown", ago(1), 0, ago(1), 1, "", "", "", ago(1)},
			{"fact-006", "node-g-11", "gpu.count", "8", "块", "bmc-inventory", "authoritative", ago(60), 0, ago(60), 1, "", "", "", ago(60)},
			{"fact-007", "node-n-01", "identity.status", "unverified", "", "discovery-agent", "candidate", ago(1), 0, ago(1), 1, "", "", "", ago(1)},
			{"fact-008", "node-041", "spec.version", "v12", "", "ops-core", "authoritative", ago(13), 0, ago(13), 1, "", "", "", ago(13)},
		}},

		// 6. 关系（relation_kind 只能取三种合法值）。
		{table: "relations", cols: []string{
			"relation_id", "relation_kind", "source_ref", "target_ref",
			"valid_from", "valid_to", "created_at",
		}, rows: [][]any{
			{"rel-001", "physical_component", "node-041", "nic-041-1", ago(75), 0, ago(75)},
			{"rel-002", "physical_component", "node-041", "nic-041-2", ago(75), 0, ago(75)},
			{"rel-003", "network_link", "node-041", "sw-2", ago(75), 0, ago(75)},
			{"rel-005", "network_link", "node-112", "sw-1", ago(75), 0, ago(75)},
			{"rel-006", "physical_component", "node-g-11", "gpu-g11-01", ago(60), 0, ago(60)},
		}},

		// 7. 部件安装。
		{table: "component_installations", cols: []string{
			"installation_id", "node_ref", "component_ref", "component_kind",
			"installed_at", "removed_at", "node_identity_continuous", "created_at",
		}, rows: [][]any{
			{"inst-001", "node-041", "nic-041-1", "nic", ago(75), 0, "confirmed", ago(75)},
			{"inst-002", "node-041", "nic-041-2", "nic", ago(75), 0, "confirmed", ago(75)},
			{"inst-003", "node-041", "gpu-041-a", "gpu", ago(75), 0, "confirmed", ago(75)},
			{"inst-004", "node-112", "nic-112-1", "nic", ago(75), 0, "unknown", ago(75)},
			{"inst-005", "node-g-11", "gpu-g11-01", "gpu", ago(60), 0, "confirmed", ago(60)},
		}},

		// 8. 状态迁移（health / reachability / business_occupancy 受 CHECK 约束）。
		{table: "asset_transitions", cols: []string{
			"transition_id", "object_ref", "from_state", "to_state", "trigger_kind",
			"evidence_refs", "rule_refs", "expected_revision", "expected_ownership_epoch",
			"actor", "responsibility", "health", "reachability", "business_occupancy",
			"occurred_at", "created_at",
		}, rows: [][]any{
			{"tr-001", "node-041", "待纳管", "待复核", "纳管接收", seedJSON([]string{"admission-041"}), seedJSON([]string{"ops-core v12"}), 1, 0, "u-lee", "prod-7", "healthy", "reachable", "free", ago(13), ago(13)},
			{"tr-002", "node-041", "待复核", "待交付", "调试完成", seedJSON([]string{"ev-8801"}), seedJSON([]string{"ops-core v12"}), 10, 2, "domain-service", "prod-7", "healthy", "reachable", "free", ago(11), ago(11)},
			{"tr-003", "node-041", "待交付", "在役", "验收通过", seedJSON([]string{"ev-8821", "ev-8824"}), seedJSON([]string{"ops-core v12"}), 32, 4, "domain-service", "prod-7", "degraded", "reachable", "occupied", ago(9), ago(9)},
			{"tr-004", "node-112", "在役", "维护中", "监测代理失联", seedJSON([]string{"obs-004"}), seedJSON([]string{"self-heal-v1"}), 11, 9, "svc-selfheal", "prod-7", "unknown", "unreachable", "", ago(1), ago(1)},
			{"tr-005", "node-g-11", "在役", "借出", "借用交接确认", seedJSON([]string{"ho-004"}), seedJSON([]string{"ops-core v12"}), 40, 2, "u-wang", "AI 平台组", "healthy", "reachable", "free", ago(56), ago(56)},
			{"tr-006", "node-n-01", "待复核", "待纳管", "身份证据不足", seedJSON([]string{"identity-claim-001"}), seedJSON([]string{"ops-core v12"}), 1, 0, "discovery-agent", "prod-7", "unknown", "unknown", "", ago(1), ago(1)},
		}},

		// 9. 观测。
		{table: "observations", cols: []string{
			"observation_id", "source_ref", "source_identity", "collected_at", "coverage",
			"object_ref", "field_key", "value", "unit", "recorded_at",
		}, rows: [][]any{
			{"obs-001", "node-exporter", "node-exporter@node-041", ago(1), "nic.err.packets", "node-041", "nic.err.packets", "0.14", "%", ago(1)},
			{"obs-002", "bmc-sensor", "bmc@node-041", ago(1), "nic.temperature", "node-041", "nic.temperature", "68", "°C", ago(1)},
			{"obs-003", "switch-api", "sw-2", ago(1), "port.status", "node-041", "port.status", "up", "", ago(1)},
			{"obs-004", "heartbeat", "hb@node-112", ago(1), "reachability", "node-112", "reachability", "unreachable", "", ago(1)},
			{"obs-005", "bmc-inventory", "bmc@node-g-11", ago(60), "inventory", "node-g-11", "gpu.count", "8", "块", ago(60)},
		}},

		// 10. 验收覆盖。
		{table: "acceptance_coverage", cols: []string{
			"coverage_id", "requirement_key", "object_ref", "coverage_state",
			"not_applicable_basis", "validation_ref", "created_at", "updated_at",
		}, rows: [][]any{
			{"cov-001", "spec-v12/network", "node-041", "covered", "", "val-001", ago(13), ago(9)},
			{"cov-002", "spec-v12/storage", "node-041", "covered", "", "val-002", ago(13), ago(9)},
			{"cov-003", "spec-v12/firmware", "node-041", "uncovered", "", "", ago(13), ago(13)},
			{"cov-004", "spec-v12/network", "node-112", "unknown", "", "", ago(18), ago(1)},
			{"cov-005", "spec-v11/gpu", "node-g-11", "not_applicable", "借出资产不适用本次验收", "val-003", ago(60), ago(56)},
		}},

		// 11. 5 条故障片段。
		{table: "fault_segments", cols: []string{
			"segment_id", "fault_key", "observation_refs", "window_start", "window_end",
			"state", "created_at", "updated_at",
		}, rows: [][]any{
			{"seg-001", "node-041/nic.err.packets", seedJSON([]string{"obs-001"}), ago(1) - 3600000, ago(1), "active", ago(1), ago(1)},
			{"seg-002", "node-041/nic.temperature", seedJSON([]string{"obs-002"}), ago(1) - 3600000, ago(1), "recovered", ago(1), ago(1)},
			{"seg-003", "node-112/reachability", seedJSON([]string{"obs-004"}), ago(1) - 7200000, ago(1), "active", ago(1), ago(1)},
			{"seg-004", "node-041/port.flap", seedJSON([]string{"obs-003"}), ago(2) - 3600000, ago(2), "recurred", ago(2), ago(1)},
			{"seg-005", "node-g-11/gpu.inventory", seedJSON([]string{"obs-005"}), ago(60) - 3600000, ago(60), "recovered", ago(60), ago(56)},
		}},

		// 12. 自愈策略。
		{table: "self_heal_policies", cols: []string{
			"policy_id", "policy_key", "version", "subject", "asset_scope", "valid_from",
			"valid_until", "preconditions", "action_capability", "verification_standard",
			"per_node_window_ms", "per_node_max_writes", "cluster_max_concurrent", "state",
			"observe_only", "published_by", "published_at", "created_at", "updated_at",
		}, rows: [][]any{
			{"shp-001", "agent-restart", 1, "service:monitoring-agent", seedJSON(map[string]any{"kind": "计算节点"}), ago(30), 0,
				seedJSON([]string{"heartbeat_missing", "identity_confirmed"}), "service.restart", "新心跳与覆盖独立检查",
				1800000, 1, 5, "active", 0, "u-lee", ago(30), ago(30), ago(1)},
		}},

		// 13. 自愈执行记录。
		{table: "self_heal_executions", cols: []string{
			"execution_id", "policy_id", "object_ref", "operation_id", "state",
			"block_reason", "verification_outcome", "created_at", "updated_at",
		}, rows: [][]any{
			{"she-001", "shp-001", "node-112", "op-002", "blocked", "身份连续性未知，不允许自动写", "", ago(1), ago(1)},
		}},

		// 14. 4 个事件（父表，先于 hypotheses / proposals）。
		{table: "incidents", cols: []string{
			"incident_id", "symptoms", "impact_scope", "timeline", "state",
			"recovery_verified", "recovery_evidence", "root_cause_proven", "root_cause",
			"root_cause_evidence", "created_at", "updated_at",
		}, rows: [][]any{
			{"inc-121", "NIC 错误率升高，业务延迟抖动", seedJSON(map[string]any{"nodes": []string{"node-041"}, "service": "推理服务"}), seedJSON([]string{"16:41 告警", "16:45 假设登记"}), "investigating", 0, "", 0, "", "", ago(1), ago(1)},
			{"inc-118", "node-077 磁盘 SMART 告警", "{}", seedJSON([]string{"11-14 09:05 接管", "11-14 10:00 工单创建"}), "mitigated", 1, "工单回执 + 设备复测", 0, "", "", ago(2), ago(1)},
			{"inc-117", "node-112 心跳失联", "{}", seedJSON([]string{"11-15 15:02 失联", "11-15 15:05 自愈阻断"}), "open", 0, "", 0, "", "", ago(1), ago(1)},
			{"inc-116", "spec-v12 部署批次 2 失败", "{}", seedJSON([]string{"11-10 派发", "11-10 验证失败"}), "closed", 1, "回滚后服务恢复", 1, "镜像与固件不兼容", "独立验证记录 ev-9012", ago(5), ago(4)},
		}},

		// 15. 假设（每个 incident 1-2 条）。
		{table: "incident_hypotheses", cols: []string{
			"hypothesis_id", "incident_id", "statement", "proposed_by",
			"supporting_evidence", "counter_evidence", "state", "unknowns",
			"next_queries", "created_at", "updated_at",
		}, rows: [][]any{
			{"hyp-001", "inc-121", "网卡固件缺陷导致错误包", "model", seedJSON([]string{"fact-004"}), seedJSON([]string{"端口重协商后错误率未变化"}), "candidate", seedJSON([]string{"固件版本是否可信"}), seedJSON([]string{"查询 BMC 固件审计日志"}), ago(1), ago(1)},
			{"hyp-002", "inc-121", "上联交换机端口拥塞", "worker", seedJSON([]string{"obs-003"}), "[]", "supported", "[]", seedJSON([]string{"查询交换机端口计数器"}), ago(1), ago(1)},
			{"hyp-003", "inc-118", "磁盘背板接触不良", "human", seedJSON([]string{"tk-118"}), "[]", "refuted", "[]", "[]", ago(2), ago(1)},
			{"hyp-004", "inc-117", "监测代理进程崩溃", "model", seedJSON([]string{"obs-004"}), "[]", "candidate", seedJSON([]string{"代理版本"}), seedJSON([]string{"读取代理日志片段"}), ago(1), ago(1)},
			{"hyp-005", "inc-117", "节点身份连续性中断", "worker", seedJSON([]string{"inst-004"}), "[]", "unknown", "[]", "[]", ago(1), ago(1)},
			{"hyp-006", "inc-116", "镜像与固件版本不兼容", "model", seedJSON([]string{"ev-9012"}), "[]", "supported", "[]", "[]", ago(5), ago(4)},
		}},

		// 16. 处置提案。
		{table: "incident_proposals", cols: []string{
			"proposal_id", "incident_id", "object_refs", "capability_key", "params",
			"evidence_refs", "risk_boundary", "state", "proposed_by", "created_at",
		}, rows: [][]any{
			{"prop-001", "inc-121", seedJSON([]string{"node-041"}), "nic.reset", seedJSON(map[string]any{"dry_run": true}), seedJSON([]string{"fact-001", "hyp-001"}), "仅在业务排空窗口执行", "proposed", "model", ago(1)},
			{"prop-002", "inc-121", seedJSON([]string{"node-041"}), "switch.port.query", "{}", seedJSON([]string{"hyp-002"}), "只读查询", "admitted", "worker", ago(1)},
			{"prop-003", "inc-118", seedJSON([]string{"node-077"}), "disk.replace", "{}", seedJSON([]string{"tk-118"}), "硬件工单专项授权", "executed", "human", ago(2)},
			{"prop-004", "inc-117", seedJSON([]string{"node-112"}), "service.restart", "{}", seedJSON([]string{"obs-004"}), "身份未核对，禁止自动写", "rejected", "model", ago(1)},
			{"prop-005", "inc-116", seedJSON([]string{"node-a3-05"}), "image.rollback", "{}", seedJSON([]string{"ev-9012"}), "限定批次 2", "executed", "u-lee", ago(5)},
			{"prop-006", "inc-121", seedJSON([]string{"node-041"}), "nic.firmware.read", "{}", seedJSON([]string{"fact-004"}), "只读查询", "proposed", "model", ago(1)},
		}},

		// 17. 4 个工单。
		{table: "tickets", cols: []string{
			"ticket_id", "external_system", "external_ticket_id", "incident_id", "takeover_id",
			"assignee", "object_ref", "component_refs", "fault_evidence",
			"external_state_version", "external_state", "state", "verification_outcome",
			"residual_limitations", "follow_up_responsibility", "created_at", "updated_at",
		}, rows: [][]any{
			{"tk-118", "servicenow", "INC00118", "inc-118", "to-118", "u-wang", "node-077", seedJSON([]string{"disk-077-1"}), seedJSON([]string{"SMART-01", "ev-7712"}), 3, "closed", "verified", "设备复测通过", "无", "prod-7", ago(2), ago(1)},
			{"tk-119", "servicenow", "INC00119", "inc-121", "", "u-lee", "node-041", seedJSON([]string{"nic-041-1"}), seedJSON([]string{"ev-8821"}), 1, "open", "open", "", "待交换机读数核对", "网络组", ago(1), ago(1)},
			{"tk-120", "jira", "OPS-120", "inc-117", "", "svc-selfheal", "node-112", seedJSON([]string{"nic-112-1"}), seedJSON([]string{"obs-004"}), 2, "in_progress", "waiting", "", "未知在途效果待核对", "prod-7", ago(1), ago(1)},
			{"tk-121", "servicenow", "INC00121", "inc-116", "", "u-lee", "node-a3-05", "[]", seedJSON([]string{"ev-9012"}), 1, "resolved", "closed", "已回滚", "镜像版本待重验", "发布组", ago(5), ago(4)},
		}},

		// 18. 8 条交付记录，覆盖 accepted / ready / handover / in_service 四阶段。
		{table: "delivery_records", cols: []string{
			"delivery_id", "object_ref", "stage", "record_ref", "evidence_refs",
			"occurred_at", "created_at",
		}, rows: [][]any{
			{"dv-001", "node-041", "accepted", "val-001", seedJSON([]string{"ev-8801"}), ago(11), ago(11)},
			{"dv-002", "node-041", "ready", "delivery-041", seedJSON([]string{"ev-8821"}), ago(9), ago(9)},
			{"dv-003", "node-041", "handover", "ho-041", "[]", ago(9), ago(9)},
			{"dv-004", "node-041", "in_service", "op-041", seedJSON([]string{"ev-8824"}), ago(9), ago(9)},
			{"dv-005", "node-g-11", "accepted", "val-010", "[]", ago(60), ago(60)},
			{"dv-006", "node-g-11", "ready", "delivery-g11", "[]", ago(60), ago(60)},
			{"dv-007", "node-g-11", "handover", "ho-004", "[]", ago(56), ago(56)},
			{"dv-008", "node-112", "in_service", "op-112", "[]", ago(18), ago(18)},
			{"dv-009", "node-a3-05", "accepted", "val-019", seedJSON([]string{"ev-9010"}), ago(5), ago(5)},
			{"dv-010", "node-a3-05", "ready", "delivery-a305", seedJSON([]string{"ev-9012"}), ago(4), ago(4)},
			{"dv-011", "node-a3-05", "handover", "ho-019", "[]", ago(3), ago(3)},
			{"dv-012", "node-n-01", "accepted", "val-030", "[]", ago(1), ago(1)},
			{"dv-013", "node-n-02", "accepted", "val-031", "[]", ago(1), ago(1)},
			{"dv-014", "node-112", "accepted", "val-112", "[]", ago(18), ago(18)},
			{"dv-015", "node-112", "ready", "delivery-112", "[]", ago(18), ago(18)},
		}},

		// 18b. 设计交付六阶段事件（发现 → 配置 → 单机 → 联机 → 验收 → 交接）。
		{table: "delivery_stage_events", cols: []string{"event_id", "object_ref", "stage", "record_ref", "evidence_refs", "occurred_at", "created_at"}, rows: [][]any{
			{"dse-001", "node-041", "discovered", "asset-node-041", seedJSON([]string{"ev-8801"}), ago(14), ago(14)},
			{"dse-002", "node-041", "configured", "cfg-node-041", seedJSON([]string{"ev-8802"}), ago(13), ago(13)},
			{"dse-003", "node-041", "unit_test", "val-unit-041", seedJSON([]string{"ev-8810"}), ago(12), ago(12)},
			{"dse-004", "node-041", "group_test", "val-group-041", seedJSON([]string{"ev-8818"}), ago(11), ago(11)},
			{"dse-005", "node-041", "accepted", "dv-001", seedJSON([]string{"ev-8821"}), ago(10), ago(10)},
			{"dse-006", "node-041", "handover", "dv-003", seedJSON([]string{"ev-8824"}), ago(9), ago(9)},
			{"dse-007", "node-a3-05", "discovered", "asset-a3-05", "[]", ago(9), ago(9)},
			{"dse-008", "node-a3-05", "configured", "cfg-a3-05", "[]", ago(8), ago(8)},
			{"dse-009", "node-a3-05", "unit_test", "val-unit-a305", seedJSON([]string{"ev-9010"}), ago(7), ago(7)},
			{"dse-010", "node-a3-05", "group_test", "val-group-a305", seedJSON([]string{"ev-9011"}), ago(6), ago(6)},
			{"dse-011", "node-a3-05", "accepted", "dv-009", seedJSON([]string{"ev-9012"}), ago(5), ago(5)},
			{"dse-012", "node-a3-05", "handover", "dv-011", "[]", ago(3), ago(3)},
			{"dse-013", "node-g-11", "discovered", "asset-g-11", "[]", ago(70), ago(70)},
			{"dse-014", "node-g-11", "configured", "cfg-g-11", "[]", ago(68), ago(68)},
			{"dse-015", "node-g-11", "unit_test", "val-unit-g11", "[]", ago(66), ago(66)},
			{"dse-016", "node-g-11", "group_test", "val-group-g11", "[]", ago(64), ago(64)},
			{"dse-017", "node-g-11", "accepted", "dv-005", "[]", ago(60), ago(60)},
			{"dse-018", "node-n-01", "discovered", "asset-n-01", "[]", ago(4), ago(4)},
			{"dse-019", "node-n-01", "configured", "cfg-n-01", "[]", ago(3), ago(3)},
			{"dse-020", "node-n-01", "unit_test", "val-unit-n01", "[]", ago(2), ago(2)},
			{"dse-021", "node-n-01", "group_test", "val-group-n01", "[]", ago(1), ago(1)},
			{"dse-022", "node-n-02", "discovered", "asset-n-02", "[]", ago(3), ago(3)},
			{"dse-023", "node-n-02", "configured", "cfg-n-02", "[]", ago(2), ago(2)},
			{"dse-024", "node-n-02", "unit_test", "val-unit-n02", "[]", ago(1), ago(1)},
		}},

		// 19. 4 个借用（唯一有效借用的 object_ref 不重复）。
		{table: "loans", cols: []string{
			"loan_id", "object_ref", "handover_id", "lender", "borrower", "due_at",
			"read_only_scope", "state", "ownership_epoch_at_lend", "lent_at", "returned_at",
		}, rows: [][]any{
			{"loan-001", "node-g-11", "ho-004", "prod-7", "AI 平台组", ago(-30), seedJSON([]string{"inventory.read", "metrics.read"}), "active", 2, ago(56), 0},
			{"loan-002", "node-l-02", "ho-002", "prod-7", "算法组", ago(10), "[]", "returned", 1, ago(40), ago(10)},
			{"loan-003", "node-l-03", "ho-003", "prod-7", "测试组", ago(5), "[]", "expired", 1, ago(35), 0},
			{"loan-004", "node-l-04", "ho-001", "prod-7", "合作方", ago(-14), "[]", "active", 1, ago(7), 0},
		}},

		// 20. 对应 4 条交接。
		{table: "loan_handovers", cols: []string{
			"handover_id", "object_ref", "intent", "frozen_at", "ownership_epoch_before",
			"conflict_writes", "occupancy", "responsibility", "lender", "borrower", "due_at",
			"read_only_scope", "exception_conditions", "state", "created_at", "updated_at",
		}, rows: [][]any{
			{"ho-004", "node-g-11", "lend", ago(56), 2, "[]", "借入方负载", "AI 平台组", "prod-7", "AI 平台组", ago(-30), seedJSON([]string{"inventory.read"}), "归还前需核对在途效果", "confirmed", ago(56), ago(56)},
			{"ho-002", "node-l-02", "return", ago(10), 1, "[]", "", "prod-7", "prod-7", "算法组", 0, "[]", "", "confirmed", ago(10), ago(10)},
			{"ho-003", "node-l-03", "lend", ago(35), 1, "[]", "", "prod-7", "prod-7", "测试组", ago(5), "[]", "", "confirmed", ago(35), ago(35)},
			{"ho-001", "node-l-04", "lend", ago(7), 1, seedJSON([]string{"op-l04-1"}), "", "prod-7", "prod-7", "合作方", ago(-14), "[]", "在途效果未完全核对", "frozen", ago(7), ago(7)},
		}},

		// 21. 借用人工例外。
		{table: "loan_exceptions", cols: []string{
			"exception_id", "handover_id", "object_ref", "authorized_by", "received_by",
			"unknown_effects", "follow_up_responsibility", "created_at",
		}, rows: [][]any{
			{"lex-001", "ho-001", "node-l-04", "u-lee", "合作方接口人", seedJSON([]string{"op-l04-1 效果未知"}), "prod-7", ago(7)},
			{"lex-002", "ho-004", "node-g-11", "u-wang", "AI 平台组", seedJSON([]string{"归还窗口未确认"}), "AI 平台组", ago(56)},
		}},

		// 22. 8 条租约。
		{table: "work_leases", cols: []string{
			"lease_id", "unit_id", "task_id", "lease_generation", "worker_id", "token_id",
			"issued_at", "lease_until", "state", "reclaim_reason", "reclaimed_at", "dispatched_ops",
		}, rows: [][]any{
			{"lease-001", "unit-001", "task-001", 1, "worker-001", "tok-001", ago(1), ago(-1), "active", "", 0, 0},
			{"lease-002", "unit-002", "task-001", 1, "worker-002", "tok-002", ago(1), ago(-1), "active", "", 0, 1},
			{"lease-003", "unit-003", "task-002", 2, "worker-003", "tok-003", ago(2), ago(-1), "reclaimed", "worker 失联，已派发操作进入核对", ago(1), 1},
			{"lease-004", "unit-004", "task-002", 1, "worker-004", "tok-004", ago(3), ago(2), "returned", "", 0, 2},
			{"lease-005", "unit-005", "task-003", 3, "worker-005", "tok-005", ago(4), ago(3), "expired", "", 0, 0},
			{"lease-006", "unit-006", "task-003", 1, "worker-006", "tok-006", ago(1), ago(-1), "active", "", 0, 0},
			{"lease-007", "unit-007", "task-004", 1, "worker-007", "tok-007", ago(1), ago(-1), "active", "", 0, 0},
			{"lease-008", "unit-008", "task-004", 2, "worker-001", "tok-008", ago(1), ago(-1), "active", "", 0, 0},
		}},

		// 23. 7 个工作者实例。
		{table: "worker_instances", cols: []string{
			"worker_id", "role", "supported_kinds", "instance_version", "last_contact_at",
			"enabled", "revoked_at", "expires_at", "credential_digest", "credential_version",
			"allowed_scope", "registered_by", "created_at", "updated_at",
		}, rows: [][]any{
			{"worker-001", "action", seedJSON([]string{"action", "deterministic"}), "v3.2.1", ago(0), 1, 0, 0, "digest-001", 1, "prod-7", "bootstrap", ago(30), ago(1)},
			{"worker-002", "model", seedJSON([]string{"model"}), "v3.2.1", ago(0), 1, 0, 0, "digest-002", 1, "prod-7", "bootstrap", ago(30), ago(1)},
			{"worker-003", "action", seedJSON([]string{"action"}), "v3.1.9", ago(2), 0, ago(1), 0, "digest-003", 1, "prod-7", "bootstrap", ago(30), ago(2)},
			{"worker-004", "deterministic", seedJSON([]string{"deterministic"}), "v3.2.0", ago(0), 1, 0, 0, "digest-004", 1, "prod-7", "bootstrap", ago(30), ago(1)},
			{"worker-005", "model", seedJSON([]string{"model"}), "v3.2.1", ago(1), 1, 0, ago(-30), "digest-005", 2, "prod-7", "bootstrap", ago(30), ago(1)},
			{"worker-006", "action", seedJSON([]string{"action", "model"}), "v3.2.1", ago(0), 1, 0, 0, "digest-006", 1, "prod-7", "bootstrap", ago(30), ago(1)},
			{"worker-007", "deterministic", seedJSON([]string{"deterministic"}), "v3.2.1", ago(0), 1, 0, 0, "digest-007", 1, "prod-7", "bootstrap", ago(30), ago(1)},
		}},

		// 24. 3 条授权。
		{table: "authorizations", cols: []string{
			"auth_id", "subject", "action", "object_ref", "scope_label", "param_constraints",
			"source", "policy_version", "valid_from", "expires_at", "revoked_at", "revoked_by",
			"owner", "revision", "created_at", "updated_at",
		}, rows: [][]any{
			{"auth-001", "user:u-lee", "write", "node-041", "prod-7", "{}", "用户授权", 1, ago(30), 0, 0, "", "u-lee", 1, ago(30), ago(1)},
			{"auth-002", "policy:shp-001", "dispatch", "node-112", "prod-7", seedJSON(map[string]any{"observe_only": false}), "策略", 1, ago(30), 0, ago(1), "u-lee", "prod-7", 2, ago(30), ago(1)},
			{"auth-003", "worker:worker-001", "read", "", "prod-7", "{}", "Worker 注册", 1, ago(30), ago(-30), 0, "", "prod-7", 1, ago(30), ago(1)},
			{"auth-demo-confirm-1", "user:session", "confirm", "outputs/B03-交付验收报告.md", "prod-7", "{}", "用户授权", 12, ago(30), 0, 0, "", "u-lee", 1, ago(30), now},
			{"auth-demo-confirm-2", "user:session", "confirm", "node-041", "prod-7", "{}", "用户授权", 12, ago(30), 0, 0, "", "u-lee", 1, ago(30), now},
		}},

		// 24b. 2 条待决定确认（审批页真实数据）。
		{table: "approvals", cols: []string{
			"approval_id", "subject", "action", "object_ref", "params_digest",
			"contract_revision", "policy_version", "asset_revision", "decided_by",
			"expires_at", "decision", "decided_at", "request_key", "created_at",
		}, rows: [][]any{
			{"apv-demo-1", "user:session", "confirm", "outputs/B03-交付验收报告.md", "digest-apv-demo-1", 3, 12, 16, "", ago(-1), "pending", 0, "req-apv-demo-1", ago(0)},
			{"apv-demo-2", "user:session", "confirm", "node-041", "digest-apv-demo-2", 1, 12, 33, "", 0, "pending", 0, "req-apv-demo-2", ago(0)},
		}},

		// 25. 3 条保留策略。
		{table: "retention_policies", cols: []string{
			"policy_key", "object_kind", "retention_ms", "legal_hold", "business_hold",
			"tombstone_ms", "unknown_retention_ms", "created_at", "updated_at",
		}, rows: [][]any{
			{"rp-facts", "facts", 90 * day, 0, 1, 365 * day, 0, ago(60), ago(30)},
			{"rp-incidents", "incidents", 365 * day, 1, 0, 2 * 365 * day, 0, ago(60), ago(30)},
			{"rp-operations", "operations", 180 * day, 0, 0, 365 * day, 0, ago(60), ago(30)},
		}},

		// 26. 2 条清理队列。
		{table: "cleanup_queue", cols: []string{
			"cleanup_id", "kind", "object_ref", "source_ref", "unreadable", "source_version",
			"policy_version", "state", "retry_count", "next_retry_at", "failure_reason",
			"created_at", "updated_at", "completed_at",
		}, rows: [][]any{
			{"cl-001", "invalidate", "fact-003", "source:switch-api", 0, 1, 1, "pending", 0, 0, "", ago(1), ago(1), 0},
			{"cl-002", "delete", "node-n-01", "source:discovery", 1, 1, 1, "failed", 2, ago(-1), "源版本不匹配，等待重新核对", ago(2), ago(1), 0},
		}},

		// 27. 5 条能力。
		{table: "capabilities", cols: []string{
			"capability_id", "capability_key", "version", "kind", "input_schema", "output_schema",
			"target_kind", "execution_mode", "supports_get", "supports_cancel", "supports_events",
			"supports_materials", "idempotent", "fencing", "verified_scope", "exposed_to_model",
			"created_at", "updated_at",
		}, rows: [][]any{
			{"cap-001", "cluster.node.read", 1, "query", "{}", "{}", "node", "sync", "supported", "unsupported", "supported", "unsupported", "supported", "unknown", "只读节点清单", 1, ago(30), ago(1)},
			{"cap-002", "service.restart", 1, "action", "{}", "{}", "node", "async", "supported", "supported", "unknown", "unknown", "supported", "supported", "有界自愈窗口", 0, ago(30), ago(1)},
			{"cap-003", "nic.reset", 1, "action", "{}", "{}", "nic", "sync", "unknown", "unknown", "unknown", "unknown", "unknown", "unknown", "", 0, ago(30), ago(30)},
			{"cap-004", "switch.counter.query", 1, "query", "{}", "{}", "switch", "sync", "supported", "unsupported", "unsupported", "unsupported", "supported", "unknown", "交换机只读计数器", 1, ago(30), ago(1)},
			{"cap-005", "image.deploy", 1, "action", "{}", "{}", "node", "async", "supported", "supported", "supported", "supported", "supported", "supported", "部署批次", 0, ago(30), ago(5)},
		}},

		// 28. 2 个提供者（父表，先于 connections）。
		{table: "capability_providers", cols: []string{
			"provider_id", "provider_key", "version", "kind", "config_ref", "capabilities", "created_at",
		}, rows: [][]any{
			{"prov-001", "internal-runtime", 1, "internal", "configs/internal.json", seedJSON([]string{"cluster.node.read:1", "service.restart:1"}), ago(30)},
			{"prov-002", "mcp-switch", 1, "mcp", "configs/mcp-switch.json", seedJSON([]string{"switch.port.query:1"}), ago(30)},
		}},

		// 29. 4 条连接。
		{table: "capability_connections", cols: []string{
			"connection_id", "provider_id", "connection_name", "connection_version",
			"credential_ref", "enabled", "created_at",
		}, rows: [][]any{
			{"conn-001", "prov-001", "primary", 1, "cred-internal", 1, ago(30)},
			{"conn-002", "prov-001", "standby", 1, "cred-internal", 1, ago(30)},
			{"conn-003", "prov-002", "sw-2", 2, "cred-sw2", 1, ago(30)},
			{"conn-004", "prov-002", "sw-1", 1, "cred-sw1", 0, ago(30)},
		}},

		// 29b. 演示运行时连接与工具（能力接入 / 工具权限 / 调用记录）。
		{table: "connections", cols: []string{
			"id", "name", "config", "enabled", "revision", "status", "detail", "protocol", "credential_binding", "checked_at",
		}, rows: [][]any{
			{"conn-runtime-1", "内部运行时", `{"transport":"stdio","command":"/bin/true"}`, 1, 1, "ready", "", "stdio", "", ago(1)},
		}},
		{table: "connection_tools", cols: []string{"connection_id", "name", "definition", "allowed"}, rows: [][]any{
			{"conn-runtime-1", "cluster.node.read", `{"name":"cluster.node.read","title":"读取节点","description":"只读节点信息","input_schema":{"type":"object","properties":{}},"version":"1"}`, 1},
			{"conn-runtime-1", "service.restart", `{"name":"service.restart","title":"重启服务","description":"写入能力，需确认","input_schema":{"type":"object","properties":{}},"version":"1"}`, 0},
		}},
		{table: "connection_calls", cols: []string{"id", "connection_id", "connection_revision", "tool_name", "tool_version", "status", "created_at", "evidence"}, rows: [][]any{
			{"call-demo-1", "conn-runtime-1", 1, "cluster.node.read", "1", "completed", ago(1), nil},
		}},

		// 30. 4 个运维案例（父表，先于 case_events）。
		{table: "operational_cases", cols: []string{
			"case_id", "symptom", "object_ref", "environment", "hypothesis", "actual_action",
			"independent_outcome", "unknowns", "source_refs", "applicability",
			"contraindications", "invalidation_conditions", "counterexamples", "evidence_refs",
			"review_at", "root_cause", "root_cause_proven", "state", "version", "supersedes",
			"superseded_reason", "published_by", "published_at", "created_at", "updated_at",
		}, rows: [][]any{
			{"case-001", "NIC 错误率升高", "node-041", seedJSON(map[string]any{"firmware": "2.18", "kernel": "5.15"}), "固件缺陷", "按工单回滚固件", "独立指标恢复正常", seedJSON([]string{"回滚影响范围未评估"}), seedJSON([]string{"inc-121", "fact-004"}), seedJSON(map[string]any{"kind": "计算节点", "firmware": "2.18"}), seedJSON([]string{"借出资产", "身份未核对"}), seedJSON([]string{"固件版本变化"}), "[]", seedJSON([]string{"ev-8821", "ev-8824"}), ago(-30), "固件 2.18 缺陷", 1, "published", 1, "", "", "u-lee", ago(10), ago(10), ago(1)},
			{"case-002", "心跳失联", "node-112", "{}", "监测代理崩溃", "等待人工核对", "未形成独立结果", seedJSON([]string{"代理版本", "在途写效果"}), seedJSON([]string{"inc-117"}), "{}", "[]", "[]", "[]", "[]", ago(-14), "", 0, "candidate", 1, "", "", "", 0, ago(1), ago(1)},
			{"case-003", "NIC 错误率升高", "node-041", seedJSON(map[string]any{"firmware": "2.20"}), "交换机端口拥塞", "调整端口队列", "错误率恢复且无固件变更", seedJSON([]string{"队列参数长期影响"}), seedJSON([]string{"inc-121", "hyp-002"}), seedJSON(map[string]any{"kind": "计算节点"}), seedJSON([]string{"固件 2.18"}), seedJSON([]string{"拓扑变化"}), seedJSON([]string{"case-001"}), seedJSON([]string{"switch-counter-8821"}), ago(-30), "端口队列拥塞", 1, "published", 2, "case-001", "固件回滚假设被反证推翻", "u-lee", ago(3), ago(3), ago(1)},
			{"case-004", "部署批次验证失败", "node-a3-05", seedJSON(map[string]any{"image": "golden-v3"}), "镜像与固件不兼容", "回滚到 golden-v2", "独立验证通过", seedJSON([]string{"新镜像兼容矩阵"}), seedJSON([]string{"inc-116"}), "{}", "[]", "[]", "[]", seedJSON([]string{"ev-9012"}), ago(-14), "镜像 v3 与固件不兼容", 1, "suspended", 1, "", "", "发布组", ago(4), ago(5), ago(4)},
		}},

		// 31. 案例事件。
		{table: "case_events", cols: []string{
			"event_id", "case_id", "kind", "task_id", "object_ref", "evidence_ref", "detail", "occurred_at",
		}, rows: [][]any{
			{"ce-001", "case-001", "recalled", "task-121", "node-041", "ev-8821", "召回用于 inc-121 调查", ago(1)},
			{"ce-002", "case-001", "cited", "task-121", "node-041", "fact-004", "被假设 hyp-001 引用", ago(1)},
			{"ce-003", "case-003", "verified_valid", "task-121", "node-041", "switch-counter-8821", "独立验证有效", ago(1)},
			{"ce-004", "case-002", "filtered", "task-117", "node-112", "inc-117", "身份未知被过滤", ago(1)},
			{"ce-005", "case-004", "assembled", "task-116", "node-a3-05", "ev-9012", "装入部署复盘上下文", ago(4)},
			{"ce-006", "case-004", "verified_invalid", "task-116", "node-a3-05", "ev-9012", "镜像 v3 复测失败", ago(4)},
		}},

		// 32. 3 条 Operation；其中 deploy-* 幂等键用于还原 deployment_plans。
		{table: "operations", cols: []string{
			"operation_id", "task_id", "step_id", "capability_key", "capability_version",
			"connection_id", "target_refs", "params_digest", "authorization_ref",
			"idempotency_key", "state", "attempt_count", "current_attempt_id", "predecessor_id",
			"registered_at", "updated_at", "completed_at", "effect_refs", "failure_reason",
		}, rows: [][]any{
			{"op-001", "plan-001", "", "deploy", 1, "conn-001", seedJSON(map[string]any{
				"image_ref":     "golden-b03-v7",
				"deploy_method": "pxe+ansible",
				"batches": []map[string]any{
					{"batch_id": "batch-1", "target_refs": []string{"gpu-b03-01", "gpu-b03-02", "gpu-b03-03", "gpu-b03-05", "gpu-b03-07", "gpu-b03-08", "gpu-b03-09", "gpu-b03-11"}, "continue_rule": "失败即暂停", "state": "completed"},
					{"batch_id": "batch-2", "target_refs": []string{"gpu-b03-12", "gpu-b03-13", "gpu-b03-14", "gpu-b03-15", "gpu-b03-16", "gpu-b03-17", "gpu-b03-18", "gpu-b03-19"}, "continue_rule": "失败即暂停", "state": "dispatched"},
				},
				"targets": []map[string]any{
					{"object_ref": "node-041", "revision": 33, "ownership_epoch": 4, "state": "succeeded"},
				},
			}), "digest-deploy-001", "auth-001", "deploy-plan-001", "succeeded", 1, "", "", ago(9), ago(9), ago(9), seedJSON([]string{"ev-8824"}), ""},
			{"op-002", "inc-117", "step-001", "service.restart", 1, "conn-001", seedJSON([]map[string]any{{"object_ref": "node-112", "revision": 12, "ownership_epoch": 9}}), "digest-restart-001", "auth-002", "restart-node-112-001", "cancelled", 0, "", "", ago(1), ago(1), 0, "[]", "身份连续性未知，自动写被拒绝"},
			{"op-003", "task-116", "step-002", "image.deploy", 1, "conn-001", seedJSON([]map[string]any{{"object_ref": "node-a3-05", "revision": 19, "ownership_epoch": 7}}), "digest-deploy-002", "auth-001", "deploy-plan-002", "failed", 2, "attempt-002-2", "", ago(5), ago(4), ago(4), seedJSON([]string{"ev-9012"}), "独立验证未通过"},
		}},
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启领域演示数据事务失败：%w", err)
	}
	defer tx.Rollback()
	for _, t := range tables {
		if err := seedDomainInsert(ctx, tx, t); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交领域演示数据失败：%w", err)
	}
	return nil
}
