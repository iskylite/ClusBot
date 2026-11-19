package work

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

//go:embed runtime_records.sql
var runtimeRecordsSchema string

//go:embed runtime_inbox.sql
var runtimeInboxSchema string

//go:embed runtime_outbox.sql
var runtimeOutboxSchema string

//go:embed runtime_waits.sql
var runtimeWaitsSchema string

//go:embed runtime_timers.sql
var runtimeTimersSchema string

//go:embed runtime_schedules.sql
var runtimeSchedulesSchema string

//go:embed runtime_workers.sql
var runtimeWorkersSchema string

//go:embed runtime_leases.sql
var runtimeLeasesSchema string

//go:embed runtime_worker_reports.sql
var runtimeWorkerReportsSchema string

//go:embed runtime_quotas.sql
var runtimeQuotasSchema string

//go:embed runtime_scheduler.sql
var runtimeSchedulerSchema string

//go:embed runtime_authorization.sql
var runtimeAuthorizationSchema string

//go:embed runtime_assets.sql
var runtimeAssetsSchema string

//go:embed runtime_approvals.sql
var runtimeApprovalsSchema string

//go:embed runtime_capabilities.sql
var runtimeCapabilitiesSchema string

//go:embed runtime_capability_snapshots.sql
var runtimeCapabilitySnapshotsSchema string

//go:embed runtime_evidence.sql
var runtimeEvidenceSchema string

//go:embed runtime_operations.sql
var runtimeOperationsSchema string

//go:embed runtime_operation_jobs.sql
var runtimeOperationJobsSchema string

//go:embed runtime_delegation.sql
var runtimeDelegationSchema string

//go:embed runtime_service_events.sql
var runtimeServiceEventsSchema string

//go:embed runtime_domain_definitions.sql
var runtimeDomainDefinitionsSchema string

//go:embed runtime_identity.sql
var runtimeIdentitySchema string

//go:embed runtime_facts.sql
var runtimeFactsSchema string

//go:embed runtime_asset_transitions.sql
var runtimeAssetTransitionsSchema string

//go:embed runtime_validation.sql
var runtimeValidationSchema string

//go:embed runtime_workflows.sql
var runtimeWorkflowsSchema string

//go:embed runtime_image_builds.sql
var runtimeImageBuildsSchema string

//go:embed runtime_observation.sql
var runtimeObservationSchema string

//go:embed runtime_incidents.sql
var runtimeIncidentsSchema string

//go:embed runtime_self_heal.sql
var runtimeSelfHealSchema string

//go:embed runtime_tickets.sql
var runtimeTicketsSchema string

//go:embed runtime_loans.sql
var runtimeLoansSchema string

//go:embed runtime_material_index.sql
var runtimeMaterialIndexSchema string

//go:embed runtime_context_assembly.sql
var runtimeContextAssemblySchema string

//go:embed runtime_provenance.sql
var runtimeProvenanceSchema string

//go:embed runtime_cleanup.sql
var runtimeCleanupSchema string

//go:embed runtime_cases.sql
var runtimeCasesSchema string

//go:embed runtime_skills.sql
var runtimeSkillsSchema string

//go:embed runtime_retention.sql
var runtimeRetentionSchema string

//go:embed runtime_domain_query.sql
var runtimeDomainQuerySchema string

const databaseSchemaVersion = 110
const modelProtocolMigrationFrom = 43
const modelKeyMigrationFrom = 44
const lastInputTokensMigrationFrom = 45
const contextManifestRevisionsMigrationFrom = 46
const runtimeRecordsMigrationFrom = 47
const runtimeInboxMigrationFrom = 48
const runtimeOutboxMigrationFrom = 49
const runtimeWaitsMigrationFrom = 50
const runtimeTimersMigrationFrom = 51
const runtimeSchedulesMigrationFrom = 52
const runtimeWorkersMigrationFrom = 53
const runtimeLeasesMigrationFrom = 54
const runtimeWorkerReportsMigrationFrom = 55
const runtimeQuotasMigrationFrom = 56
const runtimeSchedulerMigrationFrom = 57
const runtimeAuthorizationMigrationFrom = 58
const runtimeAssetsMigrationFrom = 59
const runtimeApprovalsMigrationFrom = 60
const runtimeCapabilitiesMigrationFrom = 61
const runtimeCapabilitySnapshotsMigrationFrom = 62
const runtimeEvidenceMigrationFrom = 63
const runtimeOperationsMigrationFrom = 64
const runtimeOperationJobsMigrationFrom = 65
const runtimeDelegationMigrationFrom = 66
const runtimeServiceEventsMigrationFrom = 67
const runtimeDomainDefinitionsMigrationFrom = 68
const runtimeIdentityMigrationFrom = 69
const runtimeFactsMigrationFrom = 70
const runtimeAssetTransitionsMigrationFrom = 71
const runtimeValidationMigrationFrom = 72
const runtimeWorkflowsMigrationFrom = 73
const runtimeImageBuildsMigrationFrom = 74
const runtimeObservationMigrationFrom = 75
const runtimeIncidentsMigrationFrom = 76
const runtimeSelfHealMigrationFrom = 77
const runtimeTicketsMigrationFrom = 78
const runtimeLoansMigrationFrom = 79
const runtimeMaterialIndexMigrationFrom = 80
const runtimeContextAssemblyMigrationFrom = 81
const runtimeProvenanceMigrationFrom = 82
const runtimeCleanupMigrationFrom = 83
const runtimeCasesMigrationFrom = 84
const runtimeSkillsMigrationFrom = 85
const runtimeRetentionMigrationFrom = 86

var (
	ErrConflict        = errors.New("请求标识已用于其他内容")
	ErrActive          = errors.New("当前对话还有一次执行正在处理")
	ErrState           = errors.New("当前状态不允许此操作")
	ErrDeliveryPending = errors.New("请先将当前成果保存到指定的交付文件")
	ErrDeliveryChanged = errors.New("交付文件缺失或内容已变更。恢复原内容，或在补充要求中改用新的文件名")
)

type Store struct {
	db               *sql.DB // 写连接（单写职责），由 writer 持有
	writer           *writerDB
	readPool         *readPool
	workspace        *workspace
	materials        *workspace
	uploads          *os.Root
	publicationMu    sync.Mutex
	connectionKey    []byte
	finalizationMu   sync.Mutex
	finalizations    map[runFinalizationKey]*runFinalization
	admissionMu      sync.Mutex
	admissions       map[string]*admissionFlight
	runtimeMu        sync.Mutex
	modelConcurrency int
	modelActive      int
	modelAvailable   chan struct{}
	admissionLimits  RunLimits
}

type Task struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	Title          string `json:"title"`
	Goal           string `json:"goal"`
	Scope          string `json:"scope_label"`
	Criteria       string `json:"acceptance_criteria,omitempty"`
	RequiredFile   string `json:"required_filename,omitempty"`
	Status         string `json:"status"`
	StatusReason   string `json:"status_reason"`
	Reason         string `json:"reason"`
	StateChangedAt int64  `json:"state_changed_at"`

	Revision            int    `json:"revision"`
	UpdatedAt           int64  `json:"updated_at"`
	CompletedAt         *int64 `json:"completed_at,omitempty"`
	RequirementsChanged bool   `json:"requirements_changed,omitempty"`
	Priority            string `json:"priority"`
}

type Run struct {
	ID                          string                     `json:"id"`
	ConversationID              string                     `json:"conversation_id"`
	TaskID                      string                     `json:"task_id,omitempty"`
	Status                      string                     `json:"status"`
	Response                    string                     `json:"response"`
	Error                       string                     `json:"error,omitempty"`
	ErrorCode                   string                     `json:"error_code,omitempty"`
	Usage                       *ExecutionUsage            `json:"usage,omitempty"`
	Generation                  int                        `json:"execution_generation"`
	FollowupQueued              bool                       `json:"followup_queued,omitempty"`
	BenchmarkRevisionSuggestion *BenchmarkRevisionTarget   `json:"benchmark_revision_suggestion,omitempty"`
	BenchmarkSuggestion         *BenchmarkRouterSuggestion `json:"benchmark_suggestion,omitempty"`
	RouterIntents               *[]RouterIntent            `json:"router_intents,omitempty"`
	admissionOutput             int
}

type Message struct {
	ID               int64          `json:"id,omitempty"`
	RunID            string         `json:"run_id,omitempty"`
	AssetQueryTaskID string         `json:"asset_query_task_id,omitempty"`
	Role             string         `json:"role"`
	Text             string         `json:"text"`
	Materials        []Material     `json:"materials,omitempty"`
	ReplaceMaterials bool           `json:"replace_materials,omitempty"`
	MemoryRefs       []MemoryRef    `json:"memory_refs,omitempty"`
	Incomplete       bool           `json:"incomplete,omitempty"`
	RequiredInput    bool           `json:"-"`
	Memories         []Memory       `json:"-"`
	Experience       *ExperienceRef `json:"-"`
	Skill            *SkillVersion  `json:"-"`
	Read             *TaskRead      `json:"-"`
	Reads            []TaskRead     `json:"-"`
}

type Conversation struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Archived bool   `json:"archived"`
	Revision int    `json:"revision"`
}

type Artifact struct {
	ID         string      `json:"id"`
	TaskID     string      `json:"task_id"`
	Name       string      `json:"name"`
	Content    string      `json:"content,omitempty"`
	Digest     string      `json:"sha256"`
	CreatedAt  int64       `json:"created_at"`
	RunID      string      `json:"run_id,omitempty"`
	TaskTitle  string      `json:"task_title,omitempty"`
	Scope      string      `json:"scope_label,omitempty"`
	AcceptedAt int64       `json:"accepted_at,omitempty"`
	Acceptance *Acceptance `json:"acceptance,omitempty"`
}

type Event struct {
	Seq  int64
	Kind string
	Data string
}

type Detail struct {
	Task             *Task           `json:"task,omitempty"`
	Conversation     Conversation    `json:"conversation"`
	Messages         []Message       `json:"messages"`
	Run              *Run            `json:"run,omitempty"`
	Artifact         *Artifact       `json:"artifact,omitempty"`
	ArtifactVersions []Artifact      `json:"artifact_versions,omitempty"`
	Publication      *Publication    `json:"publication,omitempty"`
	Acceptance       *Acceptance     `json:"acceptance,omitempty"`
	TaskUsage        *ExecutionUsage `json:"task_usage,omitempty"`
	MemoryRefs       []MemoryRef     `json:"memory_refs,omitempty"`
	Read             *TaskRead       `json:"read,omitempty"`
	Reads            []TaskRead      `json:"reads,omitempty"`
	ReadCheckpoint   string          `json:"read_checkpoint,omitempty"`
	UnknownReadRunID string          `json:"unknown_read_run_id,omitempty"`
	Workflow         *Workflow       `json:"workflow,omitempty"`
	WorkflowParentID string          `json:"workflow_parent_id,omitempty"`
}

type Acceptance struct {
	ArtifactID    string `json:"artifact_id"`
	Digest        string `json:"sha256"`
	Criteria      string `json:"criteria"`
	AcceptedAt    int64  `json:"accepted_at"`
	PublicationID string `json:"publication_id,omitempty"`
	Filename      string `json:"filename,omitempty"`
}

func Open(path string) (*Store, error) {
	writer, err := openWriter(path)
	if err != nil {
		return nil, err
	}
	// 只读连接池：短只读查询并发，不持有写连接（P1-01）。
	// 打开失败不阻止服务（只读池是优化，写路径仍是权威）。
	readPool, poolErr := openReadPool(path, 4)
	if poolErr != nil {
		readPool = nil
	}
	return &Store{db: writer.db, writer: writer, readPool: readPool, connectionKey: randBytes()}, nil
}

// initializeDatabase 校验并按需迁移数据库到当前 schema 版本。
//
// 迁移设计（P0-05）：每一步通过"目标列/结构是否已存在"判定是否需要执行，
// 因此对同一库重复运行是幂等的；迁移在单个可回滚事务内完成，失败不产生
// 半套被当作有效的结构。user_version 是唯一的迁移进度标记：任何一步失败
// 都随事务回滚，库保持原版本，可诊断、可重试，不出现"部分迁移已提交"。
//
// 启动顺序的约束由 cmd/clusbot/main.go 保证：LockDataDir → Open(校验/迁移)
// → Recover* → 启动调度；本函数失败时 Open 失败，调度不会启动。
func initializeDatabase(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	// 每个迁移步骤：from 是触发它的前一版本，apply 在目标结构缺失时执行。
	// 幂等性来自结构存在性检查，而非盲目 ALTER（P0-03 发现的缺口）。
	type migration struct {
		from  int
		apply func(*sql.Tx) error
	}
	steps := []migration{
		{modelProtocolMigrationFrom, func(tx *sql.Tx) error {
			return addColumnIfMissing(tx, "model_settings", "protocol",
				`ALTER TABLE model_settings ADD COLUMN protocol TEXT NOT NULL DEFAULT 'responses' CHECK (protocol IN ('responses', 'chat_completions'))`)
		}},
		{modelKeyMigrationFrom, func(tx *sql.Tx) error {
			return addColumnIfMissing(tx, "model_settings", "api_key",
				`ALTER TABLE model_settings ADD COLUMN api_key TEXT NOT NULL DEFAULT ''`)
		}},
		{lastInputTokensMigrationFrom, func(tx *sql.Tx) error {
			return addColumnIfMissing(tx, "runs", "last_input_tokens",
				`ALTER TABLE runs ADD COLUMN last_input_tokens INTEGER`)
		}},
		{contextManifestRevisionsMigrationFrom, func(tx *sql.Tx) error {
			if err := addColumnIfMissing(tx, "context_manifests", "previous_revision",
				`ALTER TABLE context_manifests ADD COLUMN previous_revision INTEGER NOT NULL DEFAULT 0`); err != nil {
				return err
			}
			return addColumnIfMissing(tx, "context_manifests", "control_revision",
				`ALTER TABLE context_manifests ADD COLUMN control_revision INTEGER NOT NULL DEFAULT 0`)
		}},
		// P1-02：新增 Task/Step/work_unit/控制记录/结果引用/审计结构。
		// 整组结构用 CREATE TABLE IF NOT EXISTS，幂等可重入。
		{runtimeRecordsMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeRecordsSchema)
			return err
		}},
		// P1-05：新增 Inbox 结构（通用持久输入）。
		{runtimeInboxMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeInboxSchema)
			return err
		}},
		// P1-06：新增 Outbox 结构（持久待发送命令）。
		{runtimeOutboxMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeOutboxSchema)
			return err
		}},
		// P1-07：新增 Wait 结构（版本化等待）。
		{runtimeWaitsMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeWaitsSchema)
			return err
		}},
		// P1-08：新增 Timer 结构（持久计时器与逾期扫描）。
		{runtimeTimersMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeTimersSchema)
			return err
		}},
		// P1-09：新增 Schedule 结构（周期/定点触发）。
		{runtimeSchedulesMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeSchedulesSchema)
			return err
		}},
		// P2-01：新增 Worker 实例与令牌结构。
		{runtimeWorkersMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeWorkersSchema)
			return err
		}},
		// P2-02：新增租约结构（原子领取/续租/回收）。
		{runtimeLeasesMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeLeasesSchema)
			return err
		}},
		// P2-03：新增 Worker 回报与流式片段结构。
		{runtimeWorkerReportsMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeWorkerReportsSchema)
			return err
		}},
		// P2-04：新增配额预留与占用结构。
		{runtimeQuotasMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeQuotasSchema)
			return err
		}},
		// P2-05：新增公平调度与背压结构。
		{runtimeSchedulerMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeSchedulerSchema)
			return err
		}},
		// P3-01：新增授权与主体结构。
		{runtimeAuthorizationMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeAuthorizationSchema)
			return err
		}},
		// P3-02：新增资产身份与领域准入结构。
		{runtimeAssetsMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeAssetsSchema)
			return err
		}},
		// P3-03：新增确认与人工接管结构。
		{runtimeApprovalsMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeApprovalsSchema)
			return err
		}},
		// P3-04：新增能力/提供者/连接结构。
		{runtimeCapabilitiesMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeCapabilitiesSchema)
			return err
		}},
		// P3-05：新增能力快照与 Schema 发现结构。
		{runtimeCapabilitySnapshotsMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeCapabilitySnapshotsSchema)
			return err
		}},
		// P3-06：新增证据引用与读取结构。
		{runtimeEvidenceMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeEvidenceSchema)
			return err
		}},
		// P3-07：新增 Operation 与尝试结构。
		{runtimeOperationsMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeOperationsSchema)
			return err
		}},
		// P3-09：新增异步 Job 与回执结构。
		{runtimeOperationJobsMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeOperationJobsSchema)
			return err
		}},
		// P3-16：新增外部工作流委托结构。
		{runtimeDelegationMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeDelegationSchema)
			return err
		}},
		// P3-17：新增服务实例与事件结构。
		{runtimeServiceEventsMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeServiceEventsSchema)
			return err
		}},
		// P4-01：新增领域定义结构。
		{runtimeDomainDefinitionsMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeDomainDefinitionsSchema)
			return err
		}},
		// P4-02：新增身份与部件结构。
		{runtimeIdentityMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeIdentitySchema)
			return err
		}},
		// P4-03：新增双时间事实与关系结构。
		{runtimeFactsMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeFactsSchema)
			return err
		}},
		// P4-05：新增资产九态迁移结构。
		{runtimeAssetTransitionsMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeAssetTransitionsSchema)
			return err
		}},
		// P4-06：新增配置规范与验证结构。
		{runtimeValidationMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeValidationSchema)
			return err
		}},
		// P4-07：新增流程模板与绑定结构。
		{runtimeWorkflowsMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeWorkflowsSchema)
			return err
		}},
		// P4-09：新增母盘构建与核验结构。
		{runtimeImageBuildsMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeImageBuildsSchema)
			return err
		}},
		// P4-15：新增观测接入结构。
		{runtimeObservationMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeObservationSchema)
			return err
		}},
		// P4-16：新增故障调查结构。
		{runtimeIncidentsMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeIncidentsSchema)
			return err
		}},
		// P4-17：新增自愈策略结构。
		{runtimeSelfHealMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeSelfHealSchema)
			return err
		}},
		// P4-18：新增工单结构。
		{runtimeTicketsMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeTicketsSchema)
			return err
		}},
		// P4-19：新增借出与交接结构。
		{runtimeLoansMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeLoansSchema)
			return err
		}},
		// P5-01：新增材料索引结构。
		{runtimeMaterialIndexMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeMaterialIndexSchema)
			return err
		}},
		// P5-03：新增上下文装配结构。
		{runtimeContextAssemblyMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeContextAssemblySchema)
			return err
		}},
		// P5-04：新增来源依赖与撤销结构。
		{runtimeProvenanceMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeProvenanceSchema)
			return err
		}},
		// P5-05：新增异步清理结构。
		{runtimeCleanupMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeCleanupSchema)
			return err
		}},
		// P5-06：新增案例结构。
		{runtimeCasesMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeCasesSchema)
			return err
		}},
		// P5-07：新增 Skill 结构。
		{runtimeSkillsMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeSkillsSchema)
			return err
		}},
		// P5-10：新增保留与归档结构。
		{runtimeRetentionMigrationFrom, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeRetentionSchema)
			return err
		}},
		// A2+A3：新增只读领域查询任务结构。
		{87, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeDomainQuerySchema)
			return err
		}},
		// B 步：新增维护接管任务结构（同 schema 文件，幂等）。
		{88, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeDomainQuerySchema)
			return err
		}},
		{89, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeDomainQuerySchema)
			return err
		}},
		{90, func(tx *sql.Tx) error {
			_, err := tx.Exec(runtimeReadyIndex)
			return err
		}},
		{91, migrateOperationReceiptKeys},
		{92, migrateV31Benchmark},
		{93, migrateV31Discovery},
		{94, migrateV31BenchmarkHints},
		{95, migrateV31DiscoveryDrift},
		{96, migrateBenchmarkOrders},
		{97, migrateBenchmarkDrafts},
		{98, migrateBenchmarkRevisions},
		{99, migrateAttentionFeed},
		{100, migrateDiscoveryObservationCandidateIndex},
		{101, migrateBenchmarkRevisionRequests},
		{102, migrateBenchmarkAdmissionPreviews},
		{103, migrateConversationSkillBindings},
		{104, migrateConversationMCPSelection},
		{105, migrateModelProfiles},
		{106, migrateModelRoleBindings},
		{107, migrateBenchmarkRevisionModelRole},
		{108, migrateBenchmarkAdmissionConversationSource},
		{109, migrateBenchmarkExecutionControls},
	}
	for _, step := range steps {
		if version == step.from {
			if err := step.apply(tx); err != nil {
				return err
			}
			version++
		}
	}
	if version == databaseSchemaVersion {
		if _, err := tx.Exec(modelAttributesSchema); err != nil {
			return err
		}
		// 版本号可能已经是 databaseSchemaVersion（同版本幂等），或刚被推进。
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", databaseSchemaVersion)); err != nil {
			return err
		}
		return tx.Commit()
	}
	if version != 0 {
		return requireDatabaseVersion(version)
	}
	var empty bool
	if err := tx.QueryRow("SELECT NOT EXISTS(SELECT 1 FROM sqlite_schema)").Scan(&empty); err != nil {
		return err
	}
	if !empty {
		return requireDatabaseVersion(version)
	}
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	if _, err := tx.Exec(modelAttributesSchema); err != nil {
		return err
	}
	// 新建库必须包含全部当前结构：基础 schema + 各增量模块（P1-02、P1-05 等）。
	// 与增量迁移保持同一来源，禁止只改一边（P0-05.2）。
	if _, err := tx.Exec(runtimeRecordsSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeReadyIndex); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeInboxSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeOutboxSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeWaitsSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeTimersSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeSchedulesSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeWorkersSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeLeasesSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeWorkerReportsSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeQuotasSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeSchedulerSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeAuthorizationSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeAssetsSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeApprovalsSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeCapabilitiesSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeCapabilitySnapshotsSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeEvidenceSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeOperationsSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeOperationJobsSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeDelegationSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeServiceEventsSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeDomainDefinitionsSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeIdentitySchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeFactsSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeAssetTransitionsSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeValidationSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeWorkflowsSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeImageBuildsSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeObservationSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeIncidentsSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeSelfHealSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeTicketsSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeLoansSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeMaterialIndexSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeContextAssemblySchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeProvenanceSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeCleanupSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeCasesSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeSkillsSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeRetentionSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeDomainQuerySchema); err != nil {
		return err
	}
	if err := migrateV31Benchmark(tx); err != nil {
		return err
	}
	if err := migrateV31Discovery(tx); err != nil {
		return err
	}
	if err := migrateV31BenchmarkHints(tx); err != nil {
		return err
	}
	if err := migrateV31DiscoveryDrift(tx); err != nil {
		return err
	}
	if err := migrateBenchmarkOrders(tx); err != nil {
		return err
	}
	if err := migrateBenchmarkDrafts(tx); err != nil {
		return err
	}
	if err := migrateBenchmarkRevisions(tx); err != nil {
		return err
	}
	if err := migrateAttentionFeed(tx); err != nil {
		return err
	}
	if err := migrateDiscoveryObservationCandidateIndex(tx); err != nil {
		return err
	}
	if err := migrateBenchmarkRevisionRequests(tx); err != nil {
		return err
	}
	if err := migrateBenchmarkAdmissionPreviews(tx); err != nil {
		return err
	}
	if err := migrateConversationSkillBindings(tx); err != nil {
		return err
	}
	if err := migrateConversationMCPSelection(tx); err != nil {
		return err
	}
	if err := migrateModelProfiles(tx); err != nil {
		return err
	}
	if err := migrateModelRoleBindings(tx); err != nil {
		return err
	}
	if err := migrateBenchmarkRevisionModelRole(tx); err != nil {
		return err
	}
	if err := migrateBenchmarkAdmissionConversationSource(tx); err != nil {
		return err
	}
	if err := migrateBenchmarkExecutionControls(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", databaseSchemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// addColumnIfMissing 在目标列不存在时执行 ALTER；已存在则跳过（幂等）。
// 这使迁移可安全重复：首次失败后重试不会以 duplicate column 失败。
func addColumnIfMissing(tx *sql.Tx, table, column, alter string) error {
	exists, err := columnExists(tx, table, column)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = tx.Exec(alter)
	return err
}

// columnExists 报告指定表是否已包含目标列。
func columnExists(tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, rows.Err()
		}
	}
	return false, rows.Err()
}

func requireDatabaseVersion(version int) error {
	if version > databaseSchemaVersion {
		return fmt.Errorf("数据库版本为 %d，高于此程序支持的 %d；不能就地降级。请使用支持该版本的程序，或将升级前备份恢复到独立数据目录后再使用旧程序", version, databaseSchemaVersion)
	}
	if version != databaseSchemaVersion {
		return fmt.Errorf("数据库版本为 %d，当前需要 %d。请使用新的数据目录（--data-dir）", version, databaseSchemaVersion)
	}
	return nil
}

func (s *Store) SetConnectionCredentialKey(key string) {
	s.connectionKey = []byte(key)
}
func (s *Store) Close() error {
	var err error
	if s.workspace != nil {
		err = errors.Join(s.workspace.root.Close(), s.materials.root.Close())
	}
	if s.uploads != nil {
		err = errors.Join(err, s.uploads.Close())
	}
	if s.readPool != nil {
		err = errors.Join(err, s.readPool.db.Close())
	}
	return errors.Join(err, s.db.Close())
}

func id() string { return hex.EncodeToString(randBytes()) }

func randBytes() []byte {
	b := make([]byte, 16)
	rand.Read(b)
	return b
}

func digest(text string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(text))) }
func now() int64                { return time.Now().UnixMilli() }

func title(text string) string {
	line := strings.SplitN(text, "\n", 2)[0]
	runes := []rune(line)
	if len(runes) > 30 {
		return string(runes[:30]) + "…"
	}
	return line
}

func taskTitle(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	line = strings.TrimSpace(line)
	for i, punctuation := range line {
		if i == 0 || !strings.ContainsRune("。！？.!?", punctuation) {
			continue
		}
		next, size := utf8.DecodeRuneInString(line[i+utf8.RuneLen(punctuation):])
		if size == 0 || strings.ContainsRune("”’」』）】》", next) || (punctuation <= unicode.MaxASCII && !unicode.IsSpace(next)) {
			continue
		}
		// A leading list number is not a sentence. ASCII punctuation inside
		// filenames, version numbers and URLs is handled by the space check.
		if head := strings.TrimSpace(line[:i]); strings.ContainsFunc(head, unicode.IsLetter) {
			return title(head)
		}
	}
	return title(line)
}

func addEvent(tx *sql.Tx, runID, kind string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO run_events(run_id,kind,data) VALUES(?,?,?)", runID, kind, string(raw))
	return err
}

const runColumns = "id,conversation_id,coalesce(task_id,''),status,response,error,error_code,usage_tracked,model_requests,reported_requests,active_ms,time_partial,input_tokens,output_tokens,last_input_tokens,execution_generation,admission_output_used"
const taskColumns = "id,conversation_id,title,goal,scope_label,acceptance_criteria,required_filename,status,status_reason,reason,state_changed_at,revision,updated_at,completed_at,requirements_changed,priority"

type scanner interface{ Scan(...any) error }

func scanRun(row scanner) (Run, error) {
	var r Run
	var tracked bool
	var usage ExecutionUsage
	var reported int
	err := row.Scan(&r.ID, &r.ConversationID, &r.TaskID, &r.Status, &r.Response, &r.Error, &r.ErrorCode,
		&tracked, &usage.ModelRequests, &reported, &usage.ActiveMS, &usage.TimePartial, &usage.InputTokens, &usage.OutputTokens, &usage.LastInputTokens, &r.Generation, &r.admissionOutput)
	if tracked {
		usage.UnreportedRequests = usage.ModelRequests - reported
		r.Usage = &usage
	}
	return r, err
}

func scanTask(row scanner) (Task, error) {
	var t Task
	err := row.Scan(&t.ID, &t.ConversationID, &t.Title, &t.Goal, &t.Scope, &t.Criteria, &t.RequiredFile, &t.Status, &t.StatusReason,
		&t.Reason, &t.StateChangedAt, &t.Revision, &t.UpdatedAt, &t.CompletedAt, &t.RequirementsChanged, &t.Priority)
	return t, err
}

func (s *Store) Submit(ctx context.Context, requestKey, text, scope, conversationID string, task bool, materialIDs ...string) (Run, error) {
	return s.SubmitWithDelivery(ctx, requestKey, text, scope, conversationID, task, Delivery{}, materialIDs...)
}

type Delivery struct {
	Criteria         *string
	Filename         *string
	Revision         int
	Read             *TaskReadRequest
	Reads            []TaskReadRequest
	ReplaceMaterials bool
}

func (s *Store) SubmitWithDelivery(ctx context.Context, requestKey, text, scope, conversationID string, task bool, delivery Delivery, materialIDs ...string) (Run, error) {
	return s.submitWithDelivery(ctx, requestKey, text, scope, conversationID, task, delivery, nil, materialIDs)
}

func (s *Store) SubmitChat(ctx context.Context, requestKey, text, scope, conversationID string, delivery Delivery, isDelegation func(context.Context, string, []Message) bool, materialIDs ...string) (Run, error) {
	var classifier DelegationClassifier
	if isDelegation != nil {
		classifier = func(ctx context.Context, text string, history []Message, _ int) (DelegationResult, error) {
			return DelegationResult{Task: isDelegation(ctx, text, history)}, nil
		}
	}
	return s.SubmitClassifiedChat(ctx, requestKey, text, scope, conversationID, delivery, classifier, materialIDs...)
}

func (s *Store) SubmitClassifiedChat(ctx context.Context, requestKey, text, scope, conversationID string, delivery Delivery, classify DelegationClassifier, materialIDs ...string) (Run, error) {
	return s.submitWithDelivery(ctx, requestKey, text, scope, conversationID, false, delivery, classify, materialIDs)
}

func submissionFingerprint(text, scope, conversationID string, task bool) string {
	return digest(fmt.Sprintf("%q %q %q %t", text, scope, conversationID, task))
}

// fullSubmissionFingerprint 计算一次提交的完整指纹（含材料与交付要求）。
// HTTP 层与持久受理层共用同一算法：同键同请求返回原回执，同键异请求冲突。
func fullSubmissionFingerprint(text, scope, conversationID string, task bool, delivery Delivery, materialIDs []string) (string, error) {
	fingerprint := submissionFingerprint(text, scope, conversationID, task)
	if len(materialIDs) > 0 {
		fingerprint = digest(fmt.Sprintf("%s %q", fingerprint, materialIDs))
	}
	if delivery.ReplaceMaterials {
		fingerprint = digest(fingerprint + " replace materials")
	}
	if delivery.Criteria != nil {
		fingerprint = digest(fmt.Sprintf("%s %q %d", fingerprint, *delivery.Criteria, delivery.Revision))
	}
	if delivery.Filename != nil {
		fingerprint = digest(fmt.Sprintf("%s filename %q %d", fingerprint, *delivery.Filename, delivery.Revision))
	}
	if delivery.Read != nil {
		payload, err := json.Marshal(delivery.Read)
		if err != nil {
			return "", err
		}
		fingerprint = digest(fingerprint + " read " + string(payload))
	}
	if len(delivery.Reads) > 0 {
		payload, err := json.Marshal(delivery.Reads)
		if err != nil {
			return "", err
		}
		fingerprint = digest(fingerprint + " reads " + string(payload))
	}
	return fingerprint, nil
}

func (s *Store) submitWithDelivery(ctx context.Context, requestKey, text, scope, conversationID string, task bool, delivery Delivery, classify DelegationClassifier, materialIDs []string) (Run, error) {
	if len(delivery.Reads) > MaxTaskReads || (delivery.Read != nil && len(delivery.Reads) > 0) {
		return Run{}, ErrReadSelection
	}
	if delivery.Filename != nil && *delivery.Filename != "" && !validFilename(*delivery.Filename) {
		return Run{}, ErrFilename
	}
	if delivery.ReplaceMaterials && conversationID == "" {
		return Run{}, ErrState
	}
	// Preserve the request's original identity when a chat submission creates a Task.
	fingerprint, err := fullSubmissionFingerprint(text, scope, conversationID, task, delivery, materialIDs)
	if err != nil {
		return Run{}, err
	}
	changesDelivery := delivery.Criteria != nil || delivery.Filename != nil
	var classification *submissionClassification
	if classify != nil && !changesDelivery && delivery.Read == nil && len(delivery.Reads) == 0 && !delivery.ReplaceMaterials {
		previous, history, err := s.chatSubmissionContext(ctx, requestKey, fingerprint, conversationID)
		if err != nil {
			return previous, err
		}
		if previous.ID != "" {
			return previous, s.linkExistingAdmission(ctx, requestKey, previous)
		}
		if history != nil {
			decision, err := s.classifySubmission(ctx, requestKey, fingerprint, conversationID, text, history, classify)
			if err != nil {
				return Run{}, err
			}
			classification, task = &decision, decision.Task
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	previous, err := previousSubmission(tx, requestKey, fingerprint)
	if err != nil {
		return previous, err
	}
	if previous.ID != "" {
		if err := linkAdmissionToRun(tx, requestKey, previous.ID, previous.TaskID, now()); err != nil {
			return Run{}, err
		}
		return previous, tx.Commit()
	}

	if classification != nil && classification.Route != nil &&
		(classification.Route.Kind == "batch" || classification.Route.Kind == "clarify" ||
			usesRouterReceipt(classification.Route.Kind) || classification.Route.ResolveID != "") {
		run, err := submitRouterBatch(ctx, tx, requestKey, fingerprint, text, scope, conversationID, classification, materialIDs)
		if err != nil {
			return Run{}, err
		}
		// 关联受理回执（A1：分类/任务已提交时重试只返回原结果）。
		if err := linkAdmissionToRun(tx, requestKey, run.ID, run.TaskID, now()); err != nil {
			return Run{}, err
		}
		return run, tx.Commit()
	}
	run, err := submitWorkTx(ctx, tx, requestKey, fingerprint, text, scope, conversationID, task, delivery, classification, materialIDs)
	if err != nil {
		return Run{}, err
	}
	// 关联受理回执（A1：分类/任务已提交时重试只返回原结果）。
	if err := linkAdmissionToRun(tx, requestKey, run.ID, run.TaskID, now()); err != nil {
		return Run{}, err
	}
	return run, tx.Commit()
}

func submitWorkTx(ctx context.Context, tx *sql.Tx, requestKey, fingerprint, text, scope, conversationID string, task bool, delivery Delivery, classification *submissionClassification, materialIDs []string) (Run, error) {
	var err error
	reads := delivery.Reads
	if delivery.Read != nil {
		reads = []TaskReadRequest{*delivery.Read}
	}
	changesDelivery := delivery.Criteria != nil || delivery.Filename != nil

	var suggestion *BenchmarkRouterSuggestion
	var isBenchmarkSuggestion bool
	goal := text
	reply := ""
	if classification != nil && classification.Route != nil {
		route := classification.Route
		switch route.Kind {
		case "benchmark_revision_suggestion":
			isBenchmarkSuggestion = true
			reply = "已记录明确标注的基准修订要求。请打开预演核对，当前任务尚未改变。"
		case "benchmark_preview":
			isBenchmarkSuggestion = true
			actor, err := benchmarkRouterActor(ctx, tx, requestKey)
			if err != nil {
				return Run{}, err
			}
			suggestion, err = benchmarkRouterSuggestionTx(ctx, tx, actor, conversationID, text)
			if err != nil {
				return Run{}, err
			}
			if suggestion != nil && suggestion.Target == route.BenchmarkTarget && len(materialIDs) == 0 &&
				(route.BenchmarkProfile == nil || *route.BenchmarkProfile == suggestion.Profile) {
				reply = "找到 " + suggestion.Target + " 的已发布方案。请明确打开基准受理预览；尚未创建测试任务。"
			} else {
				suggestion = nil
				reply = "当前无法确认唯一可用的已发布基准方案，没有创建测试任务。请核对目标或使用基准预览入口。"
			}
		case "new_task":
			goal = route.Goal
			delivery.Criteria = &route.Criteria
		case "follow_up", "progress", "pause", "stop", "resume":
			target, err := scanTask(tx.QueryRow("SELECT "+taskColumns+" FROM tasks WHERE id=?", route.TaskID))
			if err != nil {
				return Run{}, err
			}
			switch route.Kind {
			case "follow_up":
				if target.Revision != route.Revision {
					return Run{}, ErrTaskRevision
				}
				if err := ensureGoalTaskContinuable(tx, target.ID); err != nil {
					return Run{}, err
				}
				if err := checkConversationSubmission(tx, target.ConversationID); errors.Is(err, ErrActive) && len(materialIDs) == 0 {
					pending, err := deferTaskFollowup(tx, target, requestKey, fingerprint, text)
					if err != nil {
						return Run{}, err
					}
					return pending, nil
				} else if err != nil {
					return Run{}, err
				}
				conversationID = target.ConversationID
			case "progress":
				reply = target.Title + "：" + target.Reason
			case "resume":
				if _, err := resumeTaskTx(tx, target.ID, digest("router-resume:"+requestKey), route.Revision, "resume"); err != nil {
					return Run{}, err
				}
				reply = target.Title + "：已安排继续"
			case "pause", "stop":
				if err := controlTask(tx, target.ID, route.Kind, route.Revision); err != nil {
					return Run{}, err
				}
				target, err = scanTask(tx.QueryRow("SELECT "+taskColumns+" FROM tasks WHERE id=?", target.ID))
				if err != nil {
					return Run{}, err
				}
				reply = target.Title + "：" + target.Reason
			}
		case "clarify":
			reply = route.Question
		}
	}
	r := Run{ID: id(), ConversationID: conversationID, Status: "queued"}
	if reply != "" {
		r.Status, r.Response = "completed", reply
	}
	var taskID any
	reuseRead := true
	if conversationID == "" {
		r.ConversationID = id()
		if _, err = tx.Exec("INSERT INTO conversations(id,title,created_at) VALUES(?,?,?)", r.ConversationID, title(text), now()); err != nil {
			return Run{}, err
		}
		if task && !isBenchmarkSuggestion {
			r.TaskID = id()
			taskID = r.TaskID
			if err = insertTask(tx, r, goal, scope, delivery); err != nil {
				return Run{}, err
			}
		}
	} else {
		if err = checkConversationSubmission(tx, conversationID); err != nil {
			return Run{}, err
		}
		if !isBenchmarkSuggestion {
			existing, findErr := scanTask(tx.QueryRow("SELECT "+taskColumns+" FROM tasks WHERE conversation_id=?", conversationID))
			if findErr == nil {
				if err = ensureGoalTaskContinuable(tx, existing.ID); err != nil {
					return Run{}, err
				}
				if err = ensureNoPublication(tx, existing.ID); err != nil {
					return Run{}, err
				}
				if err = ensureReadConfirmed(tx, existing.ID); err != nil {
					return Run{}, err
				}
				if existing.Status == "cancelled" {
					return Run{}, ErrState
				}
				if changesDelivery && delivery.Revision != existing.Revision {
					return Run{}, ErrState
				}
				r.TaskID = existing.ID
				taskID = r.TaskID
				reuseRead = !existing.RequirementsChanged
				if _, err = tx.Exec(`UPDATE tasks SET acceptance_criteria=coalesce(?,acceptance_criteria),required_filename=coalesce(?,required_filename),
status='active',status_reason='queued',reason='补充要求已受理',requirements_changed=0,completed_at=NULL,revision=revision+1,updated_at=? WHERE id=?`, delivery.Criteria, delivery.Filename, now(), r.TaskID); err != nil {
					return Run{}, err
				}
			} else if !errors.Is(findErr, sql.ErrNoRows) {
				return Run{}, findErr
			} else if task && !delivery.ReplaceMaterials {
				r.TaskID = id()
				taskID = r.TaskID
				if err = insertTask(tx, r, goal, scope, delivery); err != nil {
					return Run{}, err
				}
			}
		}
	}
	if (changesDelivery || len(reads) > 0 || delivery.ReplaceMaterials) && r.TaskID == "" {
		return Run{}, ErrState
	}
	if _, err = tx.Exec("INSERT INTO runs(id,conversation_id,task_id,status,created_at,response,usage_tracked) VALUES(?,?,?,?,?,?,?)", r.ID, r.ConversationID, taskID, r.Status, now(), reply, reply != ""); err != nil {
		return Run{}, err
	}
	if len(reads) > 0 {
		for i, read := range reads {
			if err = recordTaskRead(ctx, tx, r, i+1, read); err != nil {
				return Run{}, err
			}
		}
	} else if r.TaskID != "" && reuseRead {
		if err = reuseTaskRead(tx, r.TaskID, r.ID, false); err != nil {
			return Run{}, err
		}
	}
	res, err := tx.Exec("INSERT INTO messages(conversation_id,run_id,role,text,replace_materials) VALUES(?,?,'user',?,?)",
		r.ConversationID, r.ID, text, delivery.ReplaceMaterials)
	if err != nil {
		return Run{}, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return Run{}, err
	}
	if err = recordInputEvent(tx, seq, requestKey); err != nil {
		return Run{}, err
	}
	if reply != "" {
		if _, err = tx.Exec("INSERT INTO messages(conversation_id,run_id,role,text) VALUES(?,?,'assistant',?)", r.ConversationID, r.ID, reply); err != nil {
			return Run{}, err
		}
		if _, err = tx.Exec("INSERT INTO input_dispositions VALUES(?,?,0,'completed',?)", seq, r.ID, now()); err != nil {
			return Run{}, err
		}
	}
	if len(materialIDs) > 0 {
		if err = attachMaterials(tx, seq, materialIDs); err != nil {
			return Run{}, err
		}
	}
	if _, err = tx.Exec("INSERT INTO submissions VALUES(?,?,?)", requestKey, fingerprint, r.ID); err != nil {
		return Run{}, err
	}
	if classification != nil {
		if err = attachClassification(tx, r.ID, requestKey, *classification); err != nil {
			return Run{}, err
		}
		if classification.Requests > 0 {
			r, err = scanRun(tx.QueryRow("SELECT "+runColumns+" FROM runs WHERE id=?", r.ID))
			if err != nil {
				return Run{}, err
			}
		}
	}
	if err = addEvent(tx, r.ID, "run.state_changed", map[string]string{"status": r.Status}); err != nil {
		return Run{}, err
	}
	if suggestion != nil {
		if err := addEvent(tx, r.ID, benchmarkRouterEvent, suggestion); err != nil {
			return Run{}, err
		}
		r.BenchmarkSuggestion = suggestion
	}
	return r, nil
}

func previousSubmission(tx *sql.Tx, requestKey, fingerprint string) (Run, error) {
	var previous, previousRun string
	err := tx.QueryRow("SELECT fingerprint,run_id FROM submissions WHERE request_key=?", requestKey).Scan(&previous, &previousRun)
	if err == nil {
		if previous != fingerprint {
			return Run{}, ErrConflict
		}
		var deferred bool
		var dispatched sql.NullString
		err = tx.QueryRow("SELECT dispatched_run_id,cancelled_at IS NULL FROM task_followups WHERE request_key=?", requestKey).Scan(&dispatched, &deferred)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Run{}, err
		}
		if dispatched.Valid {
			previousRun = dispatched.String
		}
		run, err := scanRun(tx.QueryRow("SELECT "+runColumns+" FROM runs WHERE id=?", previousRun))
		run.FollowupQueued = deferred && !dispatched.Valid
		if err != nil {
			return run, err
		}
		run.RouterIntents, err = routerIntentReceipt(tx, run.ID)
		if err == nil {
			run.BenchmarkSuggestion, err = readBenchmarkRouterReceipt(tx, run.ID)
		}
		if err == nil {
			run.BenchmarkRevisionSuggestion, err = readBenchmarkRevisionSuggestion(tx, run.ID)
		}
		return run, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Run{}, err
	}
	var classifiedFingerprint string
	err = tx.QueryRow("SELECT fingerprint FROM submission_classifications WHERE request_key=?", requestKey).Scan(&classifiedFingerprint)
	if err == nil && classifiedFingerprint != fingerprint {
		return Run{}, ErrConflict
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Run{}, err
	}
	var deleted bool
	if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM deleted_submissions WHERE request_key=?)", requestKey).Scan(&deleted); err != nil {
		return Run{}, err
	}
	if deleted {
		return Run{}, ErrConflict
	}
	return Run{}, nil
}

func checkConversationSubmission(tx *sql.Tx, id string) error {
	var archived, active bool
	err := tx.QueryRow(`SELECT archived, EXISTS(SELECT 1 FROM runs WHERE conversation_id=conversations.id
AND status IN ('queued','running')) FROM conversations WHERE id=?`, id).Scan(&archived, &active)
	if err != nil {
		return err
	}
	if archived {
		return ErrConversationArchived
	}
	if active {
		return ErrActive
	}
	return nil
}

func insertTask(tx *sql.Tx, run Run, text, scope string, delivery Delivery) error {
	_, err := tx.Exec(`INSERT INTO tasks(id,conversation_id,title,goal,scope_label,acceptance_criteria,required_filename,status,status_reason,reason,updated_at)
VALUES(?,?,?,?,?,coalesce(?,''),coalesce(?,''),'active','queued','已受理',?)`, run.TaskID, run.ConversationID, taskTitle(text), text, scope, delivery.Criteria, delivery.Filename, now())
	return err
}

func (s *Store) Conversations(archived bool) ([]Conversation, error) {
	rows, err := s.db.Query("SELECT "+conversationColumns+" FROM conversations WHERE archived=? AND NOT EXISTS(SELECT 1 FROM tasks WHERE conversation_id=conversations.id) ORDER BY created_at DESC LIMIT 100", archived)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Conversation{}
	for rows.Next() {
		item, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

type rowsQueryer interface {
	Query(string, ...any) (*sql.Rows, error)
}

func messages(q rowsQueryer, conversationID string, forModel bool) ([]Message, error) {
	return queryMessages(q, !forModel, `SELECT seq,role,text,replace_materials,run_id FROM messages WHERE conversation_id=?
AND (NOT ? OR NOT EXISTS(SELECT 1 FROM runs WHERE id=messages.run_id AND memory_command=1))
ORDER BY seq DESC LIMIT 50`, conversationID, forModel)
}

func queryMessages(q rowsQueryer, withMemoryRefs bool, selection string, args ...any) ([]Message, error) {
	rows, err := q.Query(`SELECT msg.seq,msg.role,msg.text,msg.replace_materials,msg.run_id,
		(SELECT json_group_array(json_object('id',material.id,'path',material.path,'sha256',material.sha256,
			'size',length(CAST(material.content AS BLOB)),'captured_at',material.captured_at,'upload',
			CASE WHEN upload.id IS NULL THEN NULL ELSE json_object('id',upload.id,'name',upload.name,'sha256',upload.sha256,
				'bytes',upload.size,'part',material.upload_part,'parts',upload.parts) END))
		FROM (SELECT material_id FROM message_materials WHERE message_seq=msg.seq ORDER BY ordinal) AS refs
		JOIN materials AS material ON material.id=refs.material_id LEFT JOIN uploads AS upload ON upload.id=material.upload_id),
		CASE WHEN ? AND msg.role='assistant' THEN (`+memoryRefsJSON+`msg.run_id) ELSE '[]' END,
		CASE WHEN ? AND msg.role='user' THEN coalesce((SELECT e.task_id FROM inbox_events e
        JOIN domain_query_tasks q ON q.task_id=e.task_id WHERE e.source='chat' AND e.event_id=(
            SELECT request_key FROM submissions WHERE run_id=msg.run_id LIMIT 1) LIMIT 1),'') ELSE '' END,
        msg.role='assistant' AND length(msg.text)>0 AND EXISTS(SELECT 1 FROM runs r WHERE r.id=msg.run_id AND (r.control='stop' OR r.error_code IN ('stop','stopped')))
		FROM (`+selection+`) AS msg ORDER BY msg.seq`, append([]any{withMemoryRefs, withMemoryRefs}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Message{}
	for rows.Next() {
		var item Message
		var refs, memories, runID string
		if err = rows.Scan(&item.ID, &item.Role, &item.Text, &item.ReplaceMaterials, &runID, &refs, &memories, &item.AssetQueryTaskID, &item.Incomplete); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(refs), &item.Materials); err != nil {
			return nil, err
		}
		if withMemoryRefs {
			item.RunID = runID
		}
		if withMemoryRefs && item.Role == "assistant" {
			err = json.Unmarshal([]byte(memories), &item.MemoryRefs)
			if err != nil {
				return nil, err
			}
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) Detail(key string, isTask bool) (Detail, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Detail{}, err
	}
	defer tx.Rollback()
	d := Detail{}
	if isTask {
		t, err := scanTask(tx.QueryRow("SELECT "+taskColumns+" FROM tasks WHERE id=?", key))
		if err != nil {
			return d, err
		}
		key = t.ConversationID
		d.Task = &t
	} else {
		t, err := scanTask(tx.QueryRow("SELECT "+taskColumns+" FROM tasks WHERE conversation_id=?", key))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return d, err
		}
		if err == nil {
			d.Task = &t
		}
	}
	if d.Conversation, err = scanConversation(tx.QueryRow("SELECT "+conversationColumns+" FROM conversations WHERE id=?", key)); err != nil {
		return d, err
	}
	if d.Messages, err = messages(tx, key, false); err != nil {
		return d, err
	}
	r, err := scanRun(tx.QueryRow("SELECT "+runColumns+" FROM runs WHERE conversation_id=? ORDER BY memory_command,rowid DESC LIMIT 1", key))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return d, err
	}
	if err == nil {
		d.Run = &r
		reads, readErr := listTaskReadEvidence(context.Background(), tx, r.ID)
		if readErr != nil {
			return d, readErr
		}
		if len(reads) == 1 {
			d.Read = &reads[0]
		} else {
			d.Reads = reads
		}
		if r.Status == "interrupted" || r.Status == "failed" {
			d.ReadCheckpoint, err = readResumeStatus(tx, r.ID)
			if err != nil {
				return d, err
			}
		}
		refs, readErr := readMemoryRefs(tx, r.ID)
		if readErr != nil {
			return d, readErr
		}
		d.MemoryRefs = refs
	}
	if d.Task != nil {
		graph, graphErr := readWorkflow(context.Background(), tx, d.Task.ID)
		if graphErr == nil {
			d.Workflow = &graph
		} else if !errors.Is(graphErr, sql.ErrNoRows) {
			return d, graphErr
		}
		if err = tx.QueryRow("SELECT coalesce((SELECT parent_id FROM workflow_children WHERE task_id=?),'')", d.Task.ID).
			Scan(&d.WorkflowParentID); err != nil {
			return d, err
		}
		if d.UnknownReadRunID, err = unknownReadRun(tx, d.Task.ID); err != nil {
			return d, err
		}
		usage, err := taskExecutionUsage(tx, d.Task.ID)
		if err != nil {
			return d, err
		}
		d.TaskUsage = &usage
		var a Artifact
		err = tx.QueryRow("SELECT id,task_id,name,content,sha256,created_at FROM artifacts WHERE task_id=? ORDER BY rowid DESC LIMIT 1", d.Task.ID).Scan(&a.ID, &a.TaskID, &a.Name, &a.Content, &a.Digest, &a.CreatedAt)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return d, err
		}
		if err == nil {
			d.Artifact = &a
			d.ArtifactVersions, err = listArtifacts(tx, d.Task.ID, "", "", "", 20)
			if err != nil {
				return d, err
			}
		}
		publication, readErr := scanPublication(tx.QueryRow("SELECT "+publicationColumns+" FROM publications WHERE task_id=? ORDER BY rowid DESC LIMIT 1", d.Task.ID))
		if readErr != nil && !errors.Is(readErr, sql.ErrNoRows) {
			return d, readErr
		}
		if readErr == nil && d.Artifact != nil && publication.ArtifactID == d.Artifact.ID {
			if publication.Status == "pending" && publication.PreviousDigest != "" {
				var before string
				if err = tx.QueryRow("SELECT previous_content FROM publications WHERE id=?", publication.ID).Scan(&before); err != nil {
					return d, err
				}
				publication.PreviousContent = &before
			}
			d.Publication = &publication
		}
		if d.Task.Status == "completed" && d.Artifact != nil {
			if d.Acceptance, err = artifactAcceptance(tx, d.Task.ID, d.Artifact.ID); err != nil {
				return d, err
			}
		}
	}
	return d, tx.Commit()
}

const artifactPageSize = 100

func (s *Store) Artifacts(taskID, scope, after string) ([]Artifact, error) {
	return listArtifacts(s.db, taskID, scope, after, "", artifactPageSize)
}

const artifactListColumns = `a.id,a.task_id,a.name,a.sha256,a.created_at,a.run_id,t.title,t.scope_label,
coalesce((SELECT max(accepted_at) FROM task_acceptances WHERE task_id=a.task_id AND artifact_id=a.id),0)`

func listArtifacts(q rowsQueryer, taskID, scope, after, runID string, limit int) ([]Artifact, error) {
	rows, err := q.Query("SELECT "+artifactListColumns+` FROM artifacts a JOIN tasks t ON t.id=a.task_id
WHERE (?='' OR a.task_id=?) AND (?='' OR t.scope_label=?) AND (?='' OR a.run_id=?)
AND (?='' OR a.rowid < (SELECT rowid FROM artifacts WHERE id=?))
ORDER BY a.rowid DESC LIMIT ?`, taskID, taskID, scope, scope, runID, runID, after, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Artifact{}
	for rows.Next() {
		var a Artifact
		if err = rows.Scan(&a.ID, &a.TaskID, &a.Name, &a.Digest, &a.CreatedAt, &a.RunID, &a.TaskTitle, &a.Scope, &a.AcceptedAt); err != nil {
			return nil, err
		}
		items = append(items, a)
	}
	return items, rows.Err()
}

func (s *Store) Artifact(id string) (Artifact, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Artifact{}, err
	}
	defer tx.Rollback()
	var a Artifact
	err = tx.QueryRow("SELECT "+artifactListColumns+",a.content FROM artifacts a JOIN tasks t ON t.id=a.task_id WHERE a.id=?", id).
		Scan(&a.ID, &a.TaskID, &a.Name, &a.Digest, &a.CreatedAt, &a.RunID, &a.TaskTitle, &a.Scope, &a.AcceptedAt, &a.Content)
	if err != nil {
		return a, err
	}
	if a.AcceptedAt != 0 {
		if a.Acceptance, err = artifactAcceptance(tx, a.TaskID, a.ID); err != nil {
			return a, err
		}
	}
	return a, tx.Commit()
}

func artifactAcceptance(tx *sql.Tx, taskID, artifactID string) (*Acceptance, error) {
	var a Acceptance
	err := tx.QueryRow(`SELECT a.artifact_id,a.sha256,a.criteria,a.accepted_at,coalesce(p.id,''),coalesce(p.filename,'')
FROM task_acceptances a LEFT JOIN publications p ON p.id=a.publication_id
WHERE a.task_id=? AND a.artifact_id=? ORDER BY a.revision DESC LIMIT 1`, taskID, artifactID).
		Scan(&a.ArtifactID, &a.Digest, &a.Criteria, &a.AcceptedAt, &a.PublicationID, &a.Filename)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &a, err
}

func (s *Store) Accept(ctx context.Context, taskID, artifactID string, revision int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = ensureNoPublication(tx, taskID); err != nil {
		return err
	}
	if err = ensureReadConfirmed(tx, taskID); err != nil {
		return err
	}
	var currentArtifact, runStatus, taskStatus, content, hash, requiredFile, runID string
	var currentRevision, generation int
	err = tx.QueryRow(`SELECT coalesce(a.id,''),r.status,t.status,coalesce(a.content,''),coalesce(a.sha256,''),t.required_filename,t.revision,r.id,r.execution_generation
FROM tasks t JOIN runs r ON r.task_id=t.id LEFT JOIN artifacts a ON a.run_id=r.id WHERE t.id=? ORDER BY r.rowid DESC LIMIT 1`, taskID).
		Scan(&currentArtifact, &runStatus, &taskStatus, &content, &hash, &requiredFile, &currentRevision, &runID, &generation)
	if err != nil {
		return err
	}
	if currentRevision != revision || taskStatus != "waiting_user" || currentArtifact == "" || currentArtifact != artifactID || runStatus != "completed" || digest(content) != hash {
		return ErrState
	}
	if err = checkWorkflowEvidence(tx, runID, generation); err != nil {
		return err
	}
	var publicationID any
	if requiredFile != "" {
		p, err := scanPublication(tx.QueryRow("SELECT "+publicationColumns+
			" FROM publications WHERE task_id=? AND artifact_id=? AND filename=? AND status='succeeded' ORDER BY rowid DESC LIMIT 1", taskID, artifactID, requiredFile))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrDeliveryPending
		}
		if err != nil {
			return err
		}
		status, _ := s.inspectPublication(p, false)
		if p.Digest != hash || status != "succeeded" {
			return ErrDeliveryChanged
		}
		publicationID = p.ID
	}
	acceptedAt := now()
	res, err := tx.Exec("UPDATE tasks SET status='completed',status_reason='accepted',reason='已验收成果',revision=revision+1,completed_at=?,updated_at=? WHERE id=? AND revision=? AND status='waiting_user'", acceptedAt, acceptedAt, taskID, revision)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrState
	}
	if _, err = tx.Exec(`INSERT INTO task_acceptances(task_id,revision,artifact_id,sha256,criteria,accepted_at,publication_id)
SELECT id,revision,?,?,acceptance_criteria,?,? FROM tasks WHERE id=?`, artifactID, hash, acceptedAt, publicationID, taskID); err != nil {
		return err
	}
	if err = recordAcceptedExperience(tx, taskID, artifactID, revision+1, content, hash); err != nil {
		return err
	}
	return tx.Commit()
}
