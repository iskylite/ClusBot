# RSI 在集群运维领域：研究进展与实践

> 结论优先。每条结论后标注证据强度：【一手】= 论文/官方文档原文核实；【官方】= 厂商公开博客/产品页；【推断】= 由证据推导。

## 一、一句话结论

**RSI 在集群运维领域已经跨过「概念」阶段，进入「有界自改进」的工程验证期：多个系统把「处置经验」蒸馏成可复用的 playbook / 知识条目，用双门禁防止过拟合，并已在生产验证。但没有任何一个系统把「评估器」交给 Agent 自己改——这与 ClusBot 的冻结边界设计一致。**

关键量化（均为论文自报，见出处）：

| 系统 | 机制 | 效果 | 证据 |
| --- | --- | --- | --- |
| **OpsHarness**（字节跳动 + CUHK） | 自演化 harness：对比成功/失败轨迹 → 原子提案 → **双门禁验证** | top-1 **59.0%**，比裸通用 Agent **+63.4%**，比专用 RCA Agent **4.02×** | 一手 arXiv:2608.25661 |
| **SDO**（自定义运维体） | 事故→蒸馏成 **playbook + detector**（检测器用非 LLM 代码，命中才调模型） | 复发故障 **MTTR -52%**，11 个开源应用 | 一手 arXiv:2610.14547，开源 |
| **OpsAgent**（联想生产） | 内部模型更新 + 外部经验累积的**双重自演化** | OPENRCA SOTA，已生产部署 | 一手 arXiv:2510.24145 |
| **ES Guardian Agent** | 11 阶段全生命周期 + 持久事故记忆 | 300 次自主修复循环，6 个 9 可用性目标 | 一手 arXiv:2604.03933 |
| **Google SRE「AI Insights」** | 持续从历史事故抽取洞见 + 风险分类，喂给 Agent | 官方：从确定性自动化转向 agentic AI | 官方博客（2026-05-29） |
| **阿里云 AgentLoop 经验库** | 从真实执行轨迹挖经验资产，按需召回 | 官方：降低重复试错、提升成功率 | 官方产品文档 |

---

## 二、学术进展：三条技术路线

### 路线 1：经验蒸馏成可复用资产（最主流）

这是运维领域 RSI 最落地的形态——**不改模型权重，改「操作记忆」**。

- **SDO**：事故解决后蒸馏出 `playbook`（含自然语言指引 + 可执行脚本，按角色参数化）+ `detector`（轻量非 LLM 代码，扫遥测判断该 playbook 是否适用）。关键设计：**稳态监控走代码、只有 detector 命中才付 token**，所以成本随「事故数」而不是「运行时长」增长。还维护 `architecture summary` 让 playbook 能泛化到别的应用。
- **OpsHarness**：把 harness 当作学习对象。数据面 = 分层运维知识 + idea-card 工具库；控制面 = setup / diagnosis / evolution / verification。演化时对比成功与失败轨迹，转成**原子提案**（add/update/delete × skel/op/rule/tool），**只有通过双门禁才准入**。
- **OpsAgent**：区分「内部模型更新」与「外部经验累积」两条自演化通道——这是少见的、明确把 L2（模型/策略）与 L3（知识）分开的设计。

> 对我们的意义：这条路线**天然有界**——评估器不动，只动知识。这正是 ClusBot 现在的做法（经验候选 → Skill 版本）。

### 路线 2：全生命周期自主运维（覆盖面最广）

- **ES Guardian Agent**：把 Elasticsearch 运维拆成 11 个阶段（评估/优化/部署/校准/稳定/告警/预测/自愈/学习/升级），用**多源预测性故障引擎**（metrics + 日志 + dmesg + NVMe SMART + NIC bond + 温度）提前数小时预判，再对照**持久事故记忆**预先准备处置。
- 局限：单一技术栈（ES），迁移性未验证；数字为作者自报。

### 路线 3：Agent 自身架构的自演化（最接近 RSI 本义）

- **OpsHarness** 的核心论点值得单独记：作者做了量化对比，发现**裸通用 Agent（Codex/Claude Code）已经常胜过专门从零搭的 RCA Agent**（同 backbone GPT-5.5：Codex 45% vs OpenRCA 的 RCA-Agent 32%）。所以结论是——**不要重造 Agent，要造「外部 harness」，并让 harness 自演化**。
- 这是对 ClusBot 最直接的一条借鉴：**我们的价值不在造 Agent，而在造可演化的运维 harness（工具集 + 知识 + 门禁）**。

---

## 三、工业实践：三种形态

| 厂商 | 形态 | 关键设计 |
| --- | --- | --- |
| **Google SRE** | 平台级 agentic 层 | ① AI Insights：持续审阅历史事故、抽取洞见、打风险标签，喂给调查/处置 Agent（Gemini embedding + 向量库）② 用 AI 持续**监控并改进 playbook**，还能从事故生成新 playbook ③ 明确原则：**透明优于黑盒**、Agent 必须有强身份与权限、必须能解释「为什么这么做、否决了哪些选项」 |
| **阿里云 AgentLoop** | 经验库产品 | 定位很准：**知识库让 Agent 知道「是什么」，经验库让 Agent 判断「此刻该怎么做」**；存的是支撑下一个任务的方法，不是单个任务的答案 |
| **Microsoft Azure SRE Agent** | 托管 SRE Agent 产品 | 产品化封装，面向 Azure 资源栈 |

**Google 的 8 条设计原则**（对我们最有用，官方原文）：

1. 已经自动化好的流程不必用 AI 重做
2. 新 AI 系统必须遵守现行政策流程
3. Agent 要和人一样满足安全/隐私要求
4. **Agent 必须有强身份（角色 + 权限）**
5. Agent 要有可靠性 SLO 和明确的降级/人工兜底
6. **Agent 必须能解释「为什么这么做、考虑并否决了哪些选项」——偏好透明而非黑盒**
7. 业务连续性计划必须包含 AI 失效的预案
8. AI 系统要持续按质量框架评估，并支持审计

---

## 四、共性问题（这个领域还没解决的）

| 问题 | 现状 | 证据 |
| --- | --- | --- |
| **评估器可信度** | 运维场景几乎没有形式化验证，主要靠「事故是否真被解决」这种**事后、稀疏、有延迟**的信号 | 各论文均以 benchmark + 生产案例为主，无形式化保证 |
| **防过拟合** | 只有 OpsHarness 明确提出**双门禁**；多数系统靠留出集或人工抽查 | arXiv:2608.25661 |
| **跨系统迁移** | SDO 用 architecture summary 缓解，但**没有「在 A 学的加速 B」的量化证明** | arXiv:2610.14547 |
| **成本** | SDO 明确把成本当一等约束（detector 先行）；其他系统多未披露 token 成本 | 同上 |
| **安全边界** | 多数论文不涉及「Agent 能否改自己的评估器」；Google 用「强身份 + 透明 + 兜底」而非技术冻结 | 官方博客 |

---

## 五、对 ClusBot 的四条直接借鉴

1. **harness 而非 Agent**：ClusBot 的竞争力应放在「可演化的运维 harness」——工具集、知识层、门禁，而不是重造 Agent。（OpsHarness 的量化结论支持）
2. **detector 先行降本**：SDO 的「轻量检测器常驻 + 命中才调 LLM」是控制成本的关键结构，ClusBot 的周期 Goal / 外部观测入口可直接对标。
3. **双门禁可借鉴**：ClusBot 现在是「无回归 + 无新增错误复用」单层门禁；OpsHarness 的双门禁（防过拟合 + 防回归）值得作为 P1 增强。
4. **playbook 化经验**：SDO 把经验落成「参数化 playbook + 可执行脚本」比纯文本 Skill 更可复用；ClusBot 的 Skill 可考虑引入参数化与配套检测条件。

---

## 六、出处

- OpsHarness: *From General Agents to RCA Experts: A Self-Evolving Harness for Root Cause Analysis*，arXiv:2608.25661（字节跳动 + CUHK，2026-08）
- SDO: *A Self-Defining Operator for Distributed Applications*，arXiv:2610.14547（2026，开源 github.com/self-defining-systems/sdo）
- OpsAgent: *An Evolving Multi-agent System for Incident Management in Microservices*，arXiv:2510.24145（联想生产部署）
- ES Guardian Agent: *Deploy, Calibrate, Monitor, Heal — No Human Required*，arXiv:2604.03933
- 综述: *Self-Improvements in Modern Agentic Systems: A Survey*，arXiv:2607.13104（97 页）
- Google: *AI in SRE: Where and how Google is deploying agentic AI to improve operations*（2026-05-29）
- 阿里云 AgentLoop 经验库产品文档
- Azure SRE Agent 产品页