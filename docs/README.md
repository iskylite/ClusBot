# ClusBot 设计文档

| 文档 | 内容 |
| --- | --- |
| [现行架构](architecture/README.md) | 对话如何触发任务，以及运行时、工具、回执和证据如何协作。正文链接到五张配套架构图。 |
| [RSI 有界自改进演示](presentation/ClusBot-RSI-总体架构.pptx) | Skill 提案、冻结三臂评测、正文重放与人工签核发布的闭环结构。 |
| [当前实现与联调边界](ClusBot-前后端一体设计评审.md) | 当前代码覆盖范围，及仍待实现和验证的部分。 |
| [多人账号与后端](ClusBot-单助手与多人运维实施方案.md) · [接口与联调验收](ClusBot-联调前待确认项.md) | 账号、超管任务权限、模型与绑定、助手工具、A2UI 接口和联调条件。 |
| [个人助手前端设计](ui/personal-assistant/前端设计.md) | 前端功能与交互，包括 A2UI 渲染和固定控件的分工。 |

未定稿的 SVG 和视觉规范在本地 `docs/ui/personal-assistant/` 整理，交互原型统一在 OpenPencil 中维护（不使用旧的 Pencil），历史资料保存在本地 `docs/archive/` 和 `docs/Research/`（均已标注过时警示）。

> 说明：仓库已清理未接入生产主链路的原型代码、G1 周期 Goal 与 G2 Schedule，并在 Schema v136 退役 6 张未使用的原型表（`operational_cases`、`case_events`、`cleanup_queue`、`domain_packages`、`domain_object_types`、`domain_relation_types`）。现已按 `Cluskit2-MCP` 真实 schema（`nodes`/`fanout`/`ip_range`、`NOT_FOUND`、只读 `annotations` 与 `fault_check`）对齐参数与协议，并接通 stdio 会话池、`fault_segments` 观测写入及故障五步闭环（根因假设、观察模式自愈演练与防越界验收；非观察模式自愈写操作默认不在直连 HTTP 入口开放）。对于仍无生产写入的 `facts` 与 `delivery_stage_events`，`DomainSnapshot` 会在 `unwired_sections` 中显式标注未接入。
