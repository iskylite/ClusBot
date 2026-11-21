# ClusBot V3.1 联调前待确认项

- **初始化访问。** 首位超管在网页输入用户名、密码和确认密码。部署阶段限定初始化入口只对安装人员可达，部署时验证初始化入口的实际可达范围。后端已覆盖并发提交和重启。
- **账号与资料。** 后端已强制单超管约束（仅首位超管可保持超管角色，创建/修改账号越权赋予超管返回 `409 single_superadmin_only`），`POST /api/v1/admin/users` 已支持 `must_change_password` 首次强制改密标志，`PATCH /api/v1/me/profile` 已支持成员维护显示名称（`display_name`）与头像标识（`avatar_badge`）。前端评审仍需核对超管设初始密码、忘密重置、强制改密及改密后重登的页面跳转；停用会撤销会话，但现有账号运行器没有按停用状态统一停止后台任务的保证，停用后未来派发和在途作业须继续验证。
- **任务隔离。** 联调个人消息、任务、附件、控制与实时流。若部署需要迁入旧库数据，先明确原所有者和导入范围。
- **客户端。** 服务器模式的非 API 页面目前返回 503。评审完成后实现并联调；检查账号切换清理缓存、草稿和订阅，以及任务详情返回原位置。本机与服务器模式的可用接口不同，不以本机演示代替多人链路验收。

## 资产初始化状态

`GET /api/v1/domain/discovery/setup-status` 需要登录。可同时传入 `source` 和 `scope` 筛选，单独传入其中一项返回 400。进入个人助手会话时读取该接口并按状态显示提醒，刷新时重新读取。提醒不写入对话记录。接口只读取本地配置、当前账号有权使用的启用计划和已记录的运行状态。

| state | 含义 |
| --- | --- |
| `source_unavailable` | 未安装发现来源，或该来源不支持指定范围。服务器模式仅向超级管理员开放已配置的来源。 |
| `no_active_plan` | 当前账号在可用来源上没有已启用且授权有效的匹配计划。 |
| `waiting_for_result` | 计划还没有运行记录，或最近一次处于排队、运行中。 |
| `no_observations` | 最近一次成功完成并登记了零条有效观测。 |
| `observations_available` | 最近一次成功完成并登记了有效观测。 |
| `run_failed` | 最近一次运行已记录失败。 |
| `run_skipped` | 最近一个计划时点已跳过。 |

服务器模式中的候选、计划、观测和纳管资产保存在操作者的独立业务库。发现计划每次派发前重查超管账号状态。已纳管资产支持本账号查询验收状态。

有匹配计划时返回 `plan`，包含 `id`、`source`、`scope` 和 `next_at_unix_ms`。有运行记录时还返回 `last_run`，包含 `status` 和 `scheduled_at_unix_ms`，运行成功时提供 `observation_count`。多计划按最新计划时点选择，空运行记录排在后面，再按下次运行时间和计划 ID 排序。观测详情仍用原有计划观测和候选接口。读取此状态不会访问远端来源或发起发现，零条观测只代表这一次的有效登记数。

## 模型目录与生效预览

以下路径均相对于 `/api/v1/settings/model`。在多人服务器模式由超级管理员访问。

| 接口 | 输入和回读 |
| --- | --- |
| `GET /providers`、`POST /providers`、`PUT /providers/{id}` | 写入 `name`、`protocol`、`base_url`、`auth_mode`、`enabled` 和 `revision`。`environment` 模式通过 `api_key_ref` 写入 `env:变量名`，`api_key` 模式通过同名字段写入 API Key，服务端加密保存。回环地址可设置 `none`。同一服务地址和协议下，更新时省略 `api_key` 可保留已有 API Key。更换服务地址须重新提交所用凭据，更换协议时也须重新提交直接 API Key。回读包含提供方 ID、认证模式、版本、模型数量和 `credential_status`，密钥及引用仅用于写入。跨数据目录恢复时，如未保留原实例的凭据密钥文件，需重新录入直接 API Key。 |
| `GET /catalog`、`POST /catalog`、`PUT /catalog/{id}` | 模型保存 `name`、`provider_id`、`provider_revision`、`model_id`、`revision`，可设置 `enabled` 和 `attributes`。目录条目的 `id` 为现有档案 ID，可继续用于角色绑定；同一 `provider_id` 的条目共享连接和凭据。 |
| `DELETE /providers/{id}`、`DELETE /catalog/{id}` | 提交当前 `revision`。仍有模型的提供方需要先处理所属模型。 |
| `GET /default`、`PUT /default` | 回读 `source`、`status`、`revision`。选目录模型提交 `source: "profile"`、`profile_id`、`profile_revision` 和当前选择版本 `revision`。恢复旧默认时提交 `source: "legacy_default"` 与 `revision`。用 `profile_id` 标记全局默认。 |
| `POST /providers/{id}/diagnose` | 提交提供方 `revision`。回读 `connection`、`authentication`、可选 `http_status` 以及此次检查的版本。HTTP 可达和认证状态分别显示。`not_rejected` 表示探针请求未被接口拒绝，凭据是否被提供方实际使用仍未知。 |
| `POST /providers/{id}/discover`、`POST /catalog/{id}/test` | 分别提交对应的 `revision`。发现返回可选模型和提供方有声明的能力。试答只发送固定短文本，回读 `duration_ms` 和模型版本。 |
| `GET /routing/policies`、`PUT /routing/policies/{business}/{role}` | 回读或保存模型策略。写入当前 `revision`、有序 `candidates`（每项包含 `profile_id` 和 `profile_revision`）、`allow_cross_provider` 和 `confirm_egress`。空序列用于清除本层策略。 |
| `GET /routing`、`POST /routing/preview` | 读取当前接通的角色和 Go 阶段。预览提交 `role` 及可选的 `business`，返回当前选择来源、状态、版本、可用的模型引用和已有数据出域字段。预览不调用模型。 |

`attributes` 中的 `file_input`、`tool_calling` 和 `structured_output` 使用可空布尔值（模型响应默认流式返回，不再保留冗余 `streaming` 能力开关）。字段缺省或为 `null` 表示未知，`false` 表示明确不支持，`true` 表示配置声明支持。字段回读不等同于运行能力探测。目录中的 `context_tokens` 未配置时维持未知，运行时默认窗口单独显示。

旧档案迁移保留原档案 ID、版本和角色绑定，保守地保留各自的连接归属。旧全局默认仍可使用。提供方的连接、凭据或启用状态变化后，已有角色绑定可能因版本变化需要复核。已选默认的模型或提供方失效时，接口返回不可用状态并保留原选择供修复。

生效顺序依次为业务阶段策略、同角色默认策略、原有角色绑定或全局默认。跨提供方策略需要显式确认外发范围。自动受理需要可用的全局默认，或通用路由、对话回复和任务执行三个核心阶段都已配置有效策略。候补只在无已输出内容的网络失败、限流或提供方暂时不可用时尝试，含工具定义或工具历史的模型请求固定使用首选模型。每个候选最多尝试一次，受请求次数、输出和阶段总时限限制。请求超时沿用现有服务配置。

角色接口目前覆盖 `general-router`、`conversation-reply`、`task-worker` 和基准的受理、修订、执行解释三个入口。Router、对话回复和任务执行的显式模型必须与有效默认模型使用相同协议、端点和凭据配置（可为相同的环境变量引用或无凭据）；绑定时需确认对应数据范围。档案版本、凭据或连接变化可能令绑定不可用，不能静默回退。预览是当前配置选择，`remote_observation: "not_checked"` 不是远端探测；具体一次运行仍应读取其模型回执，提供方自报标识也不是独立验证。

## 设置与权限的缺口

| 页面或行为 | 现有边界与联调要求 |
| --- | --- |
| 模型 | 多人模式下目录、默认模型和角色配置仅超管可管理；普通用户可读脱敏的当前路由及本人运行回执。提供方目录支持环境变量引用、直接 API Key 加密保存及回环地址无凭据。`GET /api/v1/assistant/token-usage-stats`（工具 `get_token_usage_stats`）已按 `24h / 7d / 30d` 与 `mine / all` 返回聚合统计。直接 API Key 和模型策略的前端表单待联调。 |
| 材料、故障流程与日志 | `UploadMaterial` 已支持 `.md/.txt/.log/.csv/.tsv/.json/.yaml/.yml`，并已提供 `GET /api/v1/materials/{id}/preview` 统一结构化预览与 `DELETE /api/v1/materials/{id}` 待发清理。故障处置五步流程与步骤终端日志已通过 `GET /api/v1/incidents/{id}/workflow` 与 `GET /api/v1/incidents/{id}/steps/{step}/logs`（及同名只读对话工具）补齐；`GET /api/v1/domain/cluster-trends` 提供 `24h / 7d / 30d` 的 UTC 时序聚合。只统计当前有权读取的节点和能归属的故障片段，历史不足时在役率返回空值。 |
| 常规 | `GET/PUT /api/v1/preferences` 读写本人语言、时区、外观、减弱动态、发送快捷键与桌面通知偏好，保存须携带当前 `revision`。浏览器和 macOS 系统通知投递待客户端联调。 |
| 执行与权限 | 现有部分授权与撤权接口各有适用范围，没有对应整页的统一在线设置接口。模型并发上限来自启动配置，当前只接受 1 或 2；样稿中的 4 不能保存为有效配置。多人模式未接通的领域授权和执行不可显示为已生效。 |
| 数据与诊断 | 本机有存储摘要与保留策略写入接口，不等于样稿中的分类清理、清理范围预览和诊断导出已完成；多人模式未开放这两个接口。不得展示虚构的已节省空间、候补原因或运行回执。 |
| Web 前端页面 (`web/src/`) | 22 页原型对应的多人网页（账号创建与管理抽屉、Composer `/` & `@` 补全与右侧「模型策略」弹出菜单、居中材料预览弹窗、右侧运维步骤终端日志抽屉、基于 ECharts/Recharts 的趋势与 Token 分布高保真图表）尚未完整接入前端代码，多人服务器非 API 页面需随前端工程一并实现与端到端联调。 |

上述待实现项联调前须决定隐藏或明确标为不可用，不能只做可点击但不会生效的控件。已接入的设置也要以服务端权限、版本和回执为准，发生冲突时保留输入并重新读取。


## 对话工具 API

- `GET /api/v1/assistant/tools` 返回基于当前身份、运行模式筛选的工具目录。工具类别是 `read`、`confirm`、`form`。
- `POST /api/v1/assistant/tools` 接受 `tool`、可选的 `params`、`query` 和 `body`。对象 ID 放在 `params`，查询参数放在 `query`，操作数据用对应业务接口的 JSON 对象。读取结果返回 `state: "completed"`、工具名和 `result`。任务是否已完成由业务结果里的状态判断。
- `POST /api/v1/assistant/interpret` 接受 `{"text":"…"}`。常用明确查询由后端直接匹配，部分涉及密码或凭据的关键词直接指向可信表单，其余请求交由当前默认模型从授权目录选择一个候选工具；该入口一次只选择一个工具。意图不明确返回 `409 assistant_intent_unresolved`，客户端保留草稿并请用户补足目标。工具结果不自动发送给意图模型。
- 助手响应可附带 `surface`，其中包含 A2UI 1.0.0 的 `createSurface` 和 `updateComponents`，消息版本为 `"1.0"`。Go 从已经鉴权的结果中提取字段并生成卡片结构。对话工具响应仍提供结构化业务字段，固定控件据此展示预览目标、参数和业务状态。确认 ID、凭据和表单导航不进入 A2UI 消息。A2UI 的详情按钮只在客户端展开当前结果。实现与组件归属见[前端设计 §7](ui/personal-assistant/前端设计.md#7-a2ui-100对话内的渲染与交互)。
- 无任务引用、附件或特殊动作的普通消息可使用 `/助手 <自然语言>`、`/assistant <自然语言>` 或对应的工具 JSON 命令；不带参数时返回工具目录。没有命令前缀时，只有请求头 `X-ClusBot-Assistant-Cards: 1` 且文本命中固定短语，才会直接产生工具结果；当前快捷查询仅覆盖当前模型、本人任务列表和待处理事项。其他普通消息继续按原 Run 协议处理。工具响应不是 Run 回执，也不自动追加到会话历史；客户端必须先支持这些响应再开启请求头。前端也可显式调用 `/assistant/interpret`。消息提交可以携带 `X-ClusBot-Reply-Strategy: deterministic_only` 以限制为明确的只读助手工具，不接收附件或模型意图判断。单轮指定模型可通过 `POST /api/v1/conversations/isolated-replies` 创建新会话或在指定会话提交，核对模型档案版本与发送当条文字的授权。
- 写操作先返回 `confirmation_required`、`confirmation_id`、`input` 和过期时间。跨账号任务另有已核验的 `target` 摘要。确认仅接受 `confirmation_id`，与**登录会话**绑定，五分钟有效，存于服务进程内，单次消费；服务重启后需要重新预览。`DELETE /api/v1/assistant/confirm/{id}` 取消待确认项。进程内待确认项上限为 128，满时返回 `429 assistant_preview_busy`。确认时会重新经过原业务接口，按该操作已有的身份、权限、版本与状态规则校验；预览本身不代表业务已受理。
- 确认 ID 在调用原业务接口前被消费。提交后断线、超时或再次收到 `409 assistant_confirmation_expired`，都不能证明操作未发生。部分工具会在预览时补入 `client_request_id` 或 `request_key` 并放进 `input`，客户端应保留原键，按该业务接口支持的查回或同键幂等规则核对；没有可查回或幂等保证的动作须先人工核实，不能自动重新预览并提交。工具调用返回 `completed` 也只表示业务接口已返回，异步任务仍看业务状态。
- 可信设置操作只返回 `form_required`、工具名和 `form` 目标，不保存配置、不执行动作。客户端只接受自身固定白名单：模型提供方、档案、角色分别指向 `/settings#model-default`、`/settings#model-profiles`、`/settings#model-roles`，MCP 连接与授权分别指向 `/connections/new`、`/connections`；账号管理和本人改密使用符号目标 `account_management`、`account_password`。这些目标不是通用跳转指令；多人页面尚未接入。服务器普通用户只开放本人改密表单，超管可打开全部表单；强制改密时直接使用改密接口。密码和密钥只能在固定可信表单提交，不能放进聊天、工具参数或模型请求；文本关键词识别不是秘密过滤保证。
- 当前模型、连接和运行回执使用单独的脱敏读取投影。多人模式下尚未接通的领域工具从目录隐藏；本机模式不提供多人账号和超管任务工具。工具目录、表单和操作结果均须按当前登录身份及运行模式重新读取。

## 联调验收

| 场景 | 过线条件 |
| --- | --- |
| 普通消息与工具 | 不带命令前缀和请求头的普通查询仍走 Run；支持卡片的客户端加请求头后，固定短语返回工具结果且不产生普通消息历史。带附件或任务引用的消息不被快捷工具截获；空命令返回目录，意图不明时请求澄清。 |
| A2UI 卡片 | 本机 Web 显示模型查询、列表及写入预览。未知版本、组件或动作走固定降级。详情展开不调用业务接口。确认、取消和可信表单走固定控件。 |
| 预览、确认和未知结果 | 确认前无业务写入。切换对话时客户端清空旧卡。不同登录会话、过期、重启或重复确认时由服务端拒绝旧确认 ID。预览后修改对象版本或撤权，确认须由原业务接口拒绝。模拟确认后断线，以原请求键或业务记录核对，验证不会重复产生副作用。 |
| 可信表单与权限 | 用合成秘密核对聊天、工具回执及模型出站请求中不含秘密；只允许固定表单提交。普通用户不能访问全局模型设置、账号管理和跨账号任务，超管只通过全站任务接口访问授权投影。必须改密的账号先改密并重新登录。 |
| 状态与部署 | 分别核对任务受理、执行、远端作业、资源占用和证据；模拟暂停与派发竞争及回执丢失，核验实际调用次数和占用。分别验证本机和服务器模式的发现与资产查询。服务器未配置发现来源时确认状态降级。 |
| 模型与设置 | 更换版本或撤销凭据后，显式角色显示不可用且不自动回退；预览和实际回执分别展示。对照回执核对偏好、业务覆盖与候补实际生效情况。 |

助手工具与确认、A2UI 结果投影、可信表单和账号业务路由位于 `internal/httpapi/`，Web 卡片位于 `web/src/`。另见[模型角色约束](../internal/model/role_bindings.go)和[派发守卫](../internal/work/runtime_operation_dispatch.go)。各验收项按本机和多人模式分别记录。
