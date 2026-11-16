package work

// V3 领域台账只读快照（GET /api/v1/domain/snapshot 的数据来源）。
//
// 契约唯一来源：docs/ui/v3-app/src/api.ts「领域台账契约」之后的 type 定义。
// 约定：字段名 snake_case；时间一律毫秒整数；JSON 字符串列保持 string 不解析；
// 所有数组字段即使为空也返回 [] 而不是 null。
//
// 实现约束：
//   - 只使用 s.db.QueryContext（Store.db 为 *sql.DB，见 store.go）；
//   - 每个查询都检查 rows.Err()，并在下一次查询前完整关闭 rows；
//   - 可空 TEXT 列用 COALESCE(col,'') 折叠后再 Scan 到 string：既不触发
//     "converting NULL to string is unsupported"，也避免误用 sql.NullString
//     时把 Valid=false 的零值当作业务空串；契约要求这些列保持 string，
//     空串即表达"未知/未关联"；
//   - 每条查询按主键或时间排序，保证稳定输出。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// DTO（View）：仅用于 JSON 序列化，不参与写路径，避免与写模型相互污染。
// 数值列统一 int64（SQLite INTEGER -> JSON number），布尔/计数列用 0/1。
// ---------------------------------------------------------------------------

type DomainAssetView struct {
	ObjectRef         string `json:"object_ref"`
	Kind              string `json:"kind"`
	Identity          string `json:"identity"`
	State             string `json:"state"`
	Revision          int64  `json:"revision"`
	OwnershipEpoch    int64  `json:"ownership_epoch"`
	FrozenAt          int64  `json:"frozen_at"`
	FrozenBy          string `json:"frozen_by"`
	FreezeReason      string `json:"freeze_reason"`
	CurrentOwner      string `json:"current_owner"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
	Health            string `json:"health"`
	Reachability      string `json:"reachability"`
	BusinessOccupancy string `json:"business_occupancy"`
}

type DomainFactView struct {
	FactID        string `json:"fact_id"`
	ObjectRef     string `json:"object_ref"`
	FieldKey      string `json:"field_key"`
	Value         string `json:"value"`
	Unit          string `json:"unit"`
	SourceRef     string `json:"source_ref"`
	Authority     string `json:"authority"`
	ValidFrom     int64  `json:"valid_from"`
	ValidTo       int64  `json:"valid_to"`
	RecordedAt    int64  `json:"recorded_at"`
	Revision      int64  `json:"revision"`
	Supersedes    string `json:"supersedes"`
	ConflictWith  string `json:"conflict_with"`
	ConflictBasis string `json:"conflict_basis"`
	CreatedAt     int64  `json:"created_at"`
}

type DomainRelationView struct {
	RelationID   string `json:"relation_id"`
	RelationKind string `json:"relation_kind"`
	SourceRef    string `json:"source_ref"`
	TargetRef    string `json:"target_ref"`
	ValidFrom    int64  `json:"valid_from"`
	ValidTo      int64  `json:"valid_to"`
	CreatedAt    int64  `json:"created_at"`
}

type DomainComponentView struct {
	InstallationID         string `json:"installation_id"`
	NodeRef                string `json:"node_ref"`
	ComponentRef           string `json:"component_ref"`
	ComponentKind          string `json:"component_kind"`
	InstalledAt            int64  `json:"installed_at"`
	RemovedAt              int64  `json:"removed_at"`
	NodeIdentityContinuous string `json:"node_identity_continuous"`
	CreatedAt              int64  `json:"created_at"`
}

type DomainTransitionView struct {
	TransitionID           string `json:"transition_id"`
	ObjectRef              string `json:"object_ref"`
	FromState              string `json:"from_state"`
	ToState                string `json:"to_state"`
	TriggerKind            string `json:"trigger_kind"`
	EvidenceRefs           string `json:"evidence_refs"`
	RuleRefs               string `json:"rule_refs"`
	ExpectedRevision       int64  `json:"expected_revision"`
	ExpectedOwnershipEpoch int64  `json:"expected_ownership_epoch"`
	Actor                  string `json:"actor"`
	Responsibility         string `json:"responsibility"`
	Health                 string `json:"health"`
	Reachability           string `json:"reachability"`
	BusinessOccupancy      string `json:"business_occupancy"`
	OccurredAt             int64  `json:"occurred_at"`
	CreatedAt              int64  `json:"created_at"`
}

type DomainCoverageView struct {
	CoverageID         string `json:"coverage_id"`
	RequirementKey     string `json:"requirement_key"`
	ObjectRef          string `json:"object_ref"`
	CoverageState      string `json:"coverage_state"`
	NotApplicableBasis string `json:"not_applicable_basis"`
	ValidationRef      string `json:"validation_ref"`
	CreatedAt          int64  `json:"created_at"`
	UpdatedAt          int64  `json:"updated_at"`
}

type DomainObjectTypeView struct {
	PackageID    string `json:"package_id"`
	TypeKey      string `json:"type_key"`
	Fields       string `json:"fields"`
	Cardinality  string `json:"cardinality"`
	IdentityRule string `json:"identity_rule"`
	DataBinding  string `json:"data_binding"`
	ConfigSpec   string `json:"config_spec"`
	TestSpec     string `json:"test_spec"`
	CreatedAt    int64  `json:"created_at"`
}

type DomainRelationTypeView struct {
	PackageID             string `json:"package_id"`
	RelationKey           string `json:"relation_key"`
	SourceType            string `json:"source_type"`
	TargetType            string `json:"target_type"`
	Cardinality           string `json:"cardinality"`
	MutuallyExclusiveWith string `json:"mutually_exclusive_with"`
	CreatedAt             int64  `json:"created_at"`
}

type DomainObservationView struct {
	ObservationID  string `json:"observation_id"`
	SourceRef      string `json:"source_ref"`
	SourceIdentity string `json:"source_identity"`
	CollectedAt    int64  `json:"collected_at"`
	Coverage       string `json:"coverage"`
	ObjectRef      string `json:"object_ref"`
	FieldKey       string `json:"field_key"`
	Value          string `json:"value"`
	Unit           string `json:"unit"`
	RecordedAt     int64  `json:"recorded_at"`
}

type DomainFaultSegmentView struct {
	SegmentID       string `json:"segment_id"`
	FaultKey        string `json:"fault_key"`
	ObservationRefs string `json:"observation_refs"`
	WindowStart     int64  `json:"window_start"`
	WindowEnd       int64  `json:"window_end"`
	State           string `json:"state"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
}

type DomainSelfHealPolicyView struct {
	PolicyID             string `json:"policy_id"`
	PolicyKey            string `json:"policy_key"`
	Version              int64  `json:"version"`
	Subject              string `json:"subject"`
	AssetScope           string `json:"asset_scope"`
	ValidFrom            int64  `json:"valid_from"`
	ValidUntil           int64  `json:"valid_until"`
	Preconditions        string `json:"preconditions"`
	ActionCapability     string `json:"action_capability"`
	VerificationStandard string `json:"verification_standard"`
	PerNodeWindowMS      int64  `json:"per_node_window_ms"`
	PerNodeMaxWrites     int64  `json:"per_node_max_writes"`
	ClusterMaxConcurrent int64  `json:"cluster_max_concurrent"`
	State                string `json:"state"`
	ObserveOnly          int64  `json:"observe_only"`
	PublishedBy          string `json:"published_by"`
	PublishedAt          int64  `json:"published_at"`
	CreatedAt            int64  `json:"created_at"`
	UpdatedAt            int64  `json:"updated_at"`
}

type DomainSelfHealExecutionView struct {
	ExecutionID         string `json:"execution_id"`
	PolicyID            string `json:"policy_id"`
	ObjectRef           string `json:"object_ref"`
	OperationID         string `json:"operation_id"`
	State               string `json:"state"`
	BlockReason         string `json:"block_reason"`
	VerificationOutcome string `json:"verification_outcome"`
	CreatedAt           int64  `json:"created_at"`
	UpdatedAt           int64  `json:"updated_at"`
}

type DomainIncidentView struct {
	IncidentID        string `json:"incident_id"`
	Symptoms          string `json:"symptoms"`
	ImpactScope       string `json:"impact_scope"`
	Timeline          string `json:"timeline"`
	State             string `json:"state"`
	RecoveryVerified  int64  `json:"recovery_verified"`
	RecoveryEvidence  string `json:"recovery_evidence"`
	RootCauseProven   int64  `json:"root_cause_proven"`
	RootCause         string `json:"root_cause"`
	RootCauseEvidence string `json:"root_cause_evidence"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
}

type DomainHypothesisView struct {
	HypothesisID       string `json:"hypothesis_id"`
	IncidentID         string `json:"incident_id"`
	Statement          string `json:"statement"`
	ProposedBy         string `json:"proposed_by"`
	SupportingEvidence string `json:"supporting_evidence"`
	CounterEvidence    string `json:"counter_evidence"`
	State              string `json:"state"`
	Unknowns           string `json:"unknowns"`
	NextQueries        string `json:"next_queries"`
	CreatedAt          int64  `json:"created_at"`
	UpdatedAt          int64  `json:"updated_at"`
}

type DomainProposalView struct {
	ProposalID    string `json:"proposal_id"`
	IncidentID    string `json:"incident_id"`
	ObjectRefs    string `json:"object_refs"`
	CapabilityKey string `json:"capability_key"`
	Params        string `json:"params"`
	EvidenceRefs  string `json:"evidence_refs"`
	RiskBoundary  string `json:"risk_boundary"`
	State         string `json:"state"`
	ProposedBy    string `json:"proposed_by"`
	CreatedAt     int64  `json:"created_at"`
}

type DomainTicketView struct {
	TicketID               string `json:"ticket_id"`
	ExternalSystem         string `json:"external_system"`
	ExternalTicketID       string `json:"external_ticket_id"`
	IncidentID             string `json:"incident_id"`
	TakeoverID             string `json:"takeover_id"`
	Assignee               string `json:"assignee"`
	ObjectRef              string `json:"object_ref"`
	ComponentRefs          string `json:"component_refs"`
	FaultEvidence          string `json:"fault_evidence"`
	ExternalStateVersion   int64  `json:"external_state_version"`
	ExternalState          string `json:"external_state"`
	State                  string `json:"state"`
	VerificationOutcome    string `json:"verification_outcome"`
	ResidualLimitations    string `json:"residual_limitations"`
	FollowUpResponsibility string `json:"follow_up_responsibility"`
	CreatedAt              int64  `json:"created_at"`
	UpdatedAt              int64  `json:"updated_at"`
}

type DomainDeliveryRecordView struct {
	DeliveryID   string `json:"delivery_id"`
	ObjectRef    string `json:"object_ref"`
	Stage        string `json:"stage"`
	RecordRef    string `json:"record_ref"`
	EvidenceRefs string `json:"evidence_refs"`
	OccurredAt   int64  `json:"occurred_at"`
	CreatedAt    int64  `json:"created_at"`
}

type DomainDeliveryStageEventView struct {
	EventID      string `json:"event_id"`
	ObjectRef    string `json:"object_ref"`
	Stage        string `json:"stage"`
	RecordRef    string `json:"record_ref"`
	EvidenceRefs string `json:"evidence_refs"`
	OccurredAt   int64  `json:"occurred_at"`
	CreatedAt    int64  `json:"created_at"`
}

type DomainDeploymentPlanView struct {
	PlanID       string `json:"plan_id"`
	ImageRef     string `json:"image_ref"`
	DeployMethod string `json:"deploy_method"`
	State        string `json:"state"`
	Batches      string `json:"batches"`
	Targets      string `json:"targets"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
}

type DomainLoanView struct {
	LoanID               string `json:"loan_id"`
	ObjectRef            string `json:"object_ref"`
	HandoverID           string `json:"handover_id"`
	Lender               string `json:"lender"`
	Borrower             string `json:"borrower"`
	DueAt                int64  `json:"due_at"`
	ReadOnlyScope        string `json:"read_only_scope"`
	State                string `json:"state"`
	OwnershipEpochAtLend int64  `json:"ownership_epoch_at_lend"`
	LentAt               int64  `json:"lent_at"`
	ReturnedAt           int64  `json:"returned_at"`
}

type DomainHandoverView struct {
	HandoverID           string `json:"handover_id"`
	ObjectRef            string `json:"object_ref"`
	Intent               string `json:"intent"`
	FrozenAt             int64  `json:"frozen_at"`
	OwnershipEpochBefore int64  `json:"ownership_epoch_before"`
	ConflictWrites       string `json:"conflict_writes"`
	Occupancy            string `json:"occupancy"`
	Responsibility       string `json:"responsibility"`
	Lender               string `json:"lender"`
	Borrower             string `json:"borrower"`
	DueAt                int64  `json:"due_at"`
	ReadOnlyScope        string `json:"read_only_scope"`
	ExceptionConditions  string `json:"exception_conditions"`
	State                string `json:"state"`
	CreatedAt            int64  `json:"created_at"`
	UpdatedAt            int64  `json:"updated_at"`
}

type DomainLoanExceptionView struct {
	ExceptionID            string `json:"exception_id"`
	HandoverID             string `json:"handover_id"`
	ObjectRef              string `json:"object_ref"`
	AuthorizedBy           string `json:"authorized_by"`
	ReceivedBy             string `json:"received_by"`
	UnknownEffects         string `json:"unknown_effects"`
	FollowUpResponsibility string `json:"follow_up_responsibility"`
	CreatedAt              int64  `json:"created_at"`
}

type DomainLeaseView struct {
	LeaseID         string `json:"lease_id"`
	UnitID          string `json:"unit_id"`
	TaskID          string `json:"task_id"`
	LeaseGeneration int64  `json:"lease_generation"`
	WorkerID        string `json:"worker_id"`
	TokenID         string `json:"token_id"`
	IssuedAt        int64  `json:"issued_at"`
	LeaseUntil      int64  `json:"lease_until"`
	State           string `json:"state"`
	ReclaimReason   string `json:"reclaim_reason"`
	ReclaimedAt     int64  `json:"reclaimed_at"`
	DispatchedOps   int64  `json:"dispatched_ops"`
}

type DomainWorkerView struct {
	WorkerID        string `json:"worker_id"`
	Role            string `json:"role"`
	SupportedKinds  string `json:"supported_kinds"`
	InstanceVersion string `json:"instance_version"`
	LastSeenAt      int64  `json:"last_seen_at"`
	Enabled         int64  `json:"enabled"`
	RevokedAt       int64  `json:"revoked_at"`
	ExpiresAt       int64  `json:"expires_at"`
	CreatedAt       int64  `json:"created_at"`
}

type DomainAuthorizationView struct {
	AuthID           string `json:"auth_id"`
	Subject          string `json:"subject"`
	Action           string `json:"action"`
	ObjectRef        string `json:"object_ref"`
	ScopeLabel       string `json:"scope_label"`
	ParamConstraints string `json:"param_constraints"`
	Source           string `json:"source"`
	PolicyVersion    int64  `json:"policy_version"`
	ValidFrom        int64  `json:"valid_from"`
	ExpiresAt        int64  `json:"expires_at"`
	RevokedAt        int64  `json:"revoked_at"`
	RevokedBy        string `json:"revoked_by"`
	Owner            string `json:"owner"`
	Revision         int64  `json:"revision"`
	CreatedAt        int64  `json:"created_at"`
	UpdatedAt        int64  `json:"updated_at"`
}

type DomainPackageView struct {
	PackageID         string `json:"package_id"`
	Name              string `json:"name"`
	Version           int64  `json:"version"`
	State             string `json:"state"`
	Definitions       string `json:"definitions"`
	DefinitionsDigest string `json:"definitions_digest"`
	Compatibility     string `json:"compatibility"`
	ImpactSummary     string `json:"impact_summary"`
	RollbackBasis     string `json:"rollback_basis"`
	PublishedBy       string `json:"published_by"`
	PublishedAt       int64  `json:"published_at"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
}

type DomainRetentionPolicyView struct {
	PolicyKey          string `json:"policy_key"`
	ObjectKind         string `json:"object_kind"`
	RetentionMS        int64  `json:"retention_ms"`
	LegalHold          int64  `json:"legal_hold"`
	BusinessHold       int64  `json:"business_hold"`
	TombstoneMS        int64  `json:"tombstone_ms"`
	UnknownRetentionMS int64  `json:"unknown_retention_ms"`
	CreatedAt          int64  `json:"created_at"`
	UpdatedAt          int64  `json:"updated_at"`
}

type DomainCleanupItemView struct {
	CleanupID     string `json:"cleanup_id"`
	Kind          string `json:"kind"`
	ObjectRef     string `json:"object_ref"`
	SourceRef     string `json:"source_ref"`
	Unreadable    int64  `json:"unreadable"`
	SourceVersion int64  `json:"source_version"`
	PolicyVersion int64  `json:"policy_version"`
	State         string `json:"state"`
	RetryCount    int64  `json:"retry_count"`
	NextRetryAt   int64  `json:"next_retry_at"`
	FailureReason string `json:"failure_reason"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

type DomainCapabilityView struct {
	CapabilityID      string `json:"capability_id"`
	CapabilityKey     string `json:"capability_key"`
	Version           int64  `json:"version"`
	Kind              string `json:"kind"`
	InputSchema       string `json:"input_schema"`
	OutputSchema      string `json:"output_schema"`
	TargetKind        string `json:"target_kind"`
	ExecutionMode     string `json:"execution_mode"`
	SupportsGet       string `json:"supports_get"`
	SupportsCancel    string `json:"supports_cancel"`
	SupportsEvents    string `json:"supports_events"`
	SupportsMaterials string `json:"supports_materials"`
	Idempotent        string `json:"idempotent"`
	Fencing           string `json:"fencing"`
	VerifiedScope     string `json:"verified_scope"`
	ExposedToModel    int64  `json:"exposed_to_model"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
}

type DomainProviderView struct {
	ProviderID   string `json:"provider_id"`
	ProviderKey  string `json:"provider_key"`
	Version      int64  `json:"version"`
	Kind         string `json:"kind"`
	ConfigRef    string `json:"config_ref"`
	Capabilities string `json:"capabilities"`
	CreatedAt    int64  `json:"created_at"`
}

type DomainCapabilityConnectionView struct {
	ConnectionID      string `json:"connection_id"`
	ProviderID        string `json:"provider_id"`
	ConnectionName    string `json:"connection_name"`
	ConnectionVersion int64  `json:"connection_version"`
	CredentialRef     string `json:"credential_ref"`
	Enabled           int64  `json:"enabled"`
	CreatedAt         int64  `json:"created_at"`
}

type DomainCaseView struct {
	CaseID                 string `json:"case_id"`
	Symptom                string `json:"symptom"`
	ObjectRef              string `json:"object_ref"`
	Environment            string `json:"environment"`
	Hypothesis             string `json:"hypothesis"`
	ActualAction           string `json:"actual_action"`
	IndependentOutcome     string `json:"independent_outcome"`
	Unknowns               string `json:"unknowns"`
	SourceRefs             string `json:"source_refs"`
	Applicability          string `json:"applicability"`
	Contraindications      string `json:"contraindications"`
	InvalidationConditions string `json:"invalidation_conditions"`
	Counterexamples        string `json:"counterexamples"`
	EvidenceRefs           string `json:"evidence_refs"`
	ReviewAt               int64  `json:"review_at"`
	RootCause              string `json:"root_cause"`
	RootCauseProven        int64  `json:"root_cause_proven"`
	State                  string `json:"state"`
	Version                int64  `json:"version"`
	Supersedes             string `json:"supersedes"`
	SupersededReason       string `json:"superseded_reason"`
	PublishedBy            string `json:"published_by"`
	PublishedAt            int64  `json:"published_at"`
	CreatedAt              int64  `json:"created_at"`
	UpdatedAt              int64  `json:"updated_at"`
}

type DomainCaseEventView struct {
	EventID     string `json:"event_id"`
	CaseID      string `json:"case_id"`
	Kind        string `json:"kind"`
	TaskID      string `json:"task_id"`
	ObjectRef   string `json:"object_ref"`
	EvidenceRef string `json:"evidence_ref"`
	Detail      string `json:"detail"`
	OccurredAt  int64  `json:"occurred_at"`
}

type DomainOperationView struct {
	OperationID       string `json:"operation_id"`
	TaskID            string `json:"task_id"`
	StepID            string `json:"step_id"`
	CapabilityKey     string `json:"capability_key"`
	CapabilityVersion int64  `json:"capability_version"`
	ConnectionID      string `json:"connection_id"`
	TargetRefs        string `json:"target_refs"`
	ParamsDigest      string `json:"params_digest"`
	AuthorizationRef  string `json:"authorization_ref"`
	IdempotencyKey    string `json:"idempotency_key"`
	State             string `json:"state"`
	AttemptCount      int64  `json:"attempt_count"`
	CurrentAttemptID  string `json:"current_attempt_id"`
	PredecessorID     string `json:"predecessor_id"`
	RegisteredAt      int64  `json:"registered_at"`
	UpdatedAt         int64  `json:"updated_at"`
	CompletedAt       int64  `json:"completed_at"`
	EffectRefs        string `json:"effect_refs"`
	FailureReason     string `json:"failure_reason"`
}

// DomainSnapshot 与 api.ts 的 DomainSnapshot 一一对应。
// 所有切片在 newDomainSnapshot 中初始化为空数组，保证 JSON 输出是 [] 而非 null。
type DomainSnapshot struct {
	SnapshotAt            int64                            `json:"snapshot_at"`
	Counts                map[string]int                   `json:"counts"`
	Assets                []DomainAssetView                `json:"assets"`
	Facts                 []DomainFactView                 `json:"facts"`
	Relations             []DomainRelationView             `json:"relations"`
	Components            []DomainComponentView            `json:"components"`
	AssetTransitions      []DomainTransitionView           `json:"asset_transitions"`
	AcceptanceCoverage    []DomainCoverageView             `json:"acceptance_coverage"`
	ObjectTypes           []DomainObjectTypeView           `json:"object_types"`
	RelationTypes         []DomainRelationTypeView         `json:"relation_types"`
	Observations          []DomainObservationView          `json:"observations"`
	FaultSegments         []DomainFaultSegmentView         `json:"fault_segments"`
	SelfHealPolicies      []DomainSelfHealPolicyView       `json:"self_heal_policies"`
	SelfHealExecutions    []DomainSelfHealExecutionView    `json:"self_heal_executions"`
	Incidents             []DomainIncidentView             `json:"incidents"`
	Hypotheses            []DomainHypothesisView           `json:"hypotheses"`
	Proposals             []DomainProposalView             `json:"proposals"`
	Tickets               []DomainTicketView               `json:"tickets"`
	DeliveryRecords       []DomainDeliveryRecordView       `json:"delivery_records"`
	DeliveryStageEvents   []DomainDeliveryStageEventView   `json:"delivery_stage_events"`
	DeploymentPlans       []DomainDeploymentPlanView       `json:"deployment_plans"`
	Loans                 []DomainLoanView                 `json:"loans"`
	Handovers             []DomainHandoverView             `json:"handovers"`
	LoanExceptions        []DomainLoanExceptionView        `json:"loan_exceptions"`
	Leases                []DomainLeaseView                `json:"leases"`
	Workers               []DomainWorkerView               `json:"workers"`
	Authorizations        []DomainAuthorizationView        `json:"authorizations"`
	DomainPackages        []DomainPackageView              `json:"domain_packages"`
	RetentionPolicies     []DomainRetentionPolicyView      `json:"retention_policies"`
	CleanupQueue          []DomainCleanupItemView          `json:"cleanup_queue"`
	Capabilities          []DomainCapabilityView           `json:"capabilities"`
	CapabilityProviders   []DomainProviderView             `json:"capability_providers"`
	CapabilityConnections []DomainCapabilityConnectionView `json:"capability_connections"`
	Cases                 []DomainCaseView                 `json:"cases"`
	CaseEvents            []DomainCaseEventView            `json:"case_events"`
	Operations            []DomainOperationView            `json:"operations"`
}

func newDomainSnapshot() DomainSnapshot {
	return DomainSnapshot{
		SnapshotAt:            time.Now().UnixMilli(),
		Counts:                map[string]int{},
		Assets:                []DomainAssetView{},
		Facts:                 []DomainFactView{},
		Relations:             []DomainRelationView{},
		Components:            []DomainComponentView{},
		AssetTransitions:      []DomainTransitionView{},
		AcceptanceCoverage:    []DomainCoverageView{},
		ObjectTypes:           []DomainObjectTypeView{},
		RelationTypes:         []DomainRelationTypeView{},
		Observations:          []DomainObservationView{},
		FaultSegments:         []DomainFaultSegmentView{},
		SelfHealPolicies:      []DomainSelfHealPolicyView{},
		SelfHealExecutions:    []DomainSelfHealExecutionView{},
		Incidents:             []DomainIncidentView{},
		Hypotheses:            []DomainHypothesisView{},
		Proposals:             []DomainProposalView{},
		Tickets:               []DomainTicketView{},
		DeliveryRecords:       []DomainDeliveryRecordView{},
		DeliveryStageEvents:   []DomainDeliveryStageEventView{},
		DeploymentPlans:       []DomainDeploymentPlanView{},
		Loans:                 []DomainLoanView{},
		Handovers:             []DomainHandoverView{},
		LoanExceptions:        []DomainLoanExceptionView{},
		Leases:                []DomainLeaseView{},
		Workers:               []DomainWorkerView{},
		Authorizations:        []DomainAuthorizationView{},
		DomainPackages:        []DomainPackageView{},
		RetentionPolicies:     []DomainRetentionPolicyView{},
		CleanupQueue:          []DomainCleanupItemView{},
		Capabilities:          []DomainCapabilityView{},
		CapabilityProviders:   []DomainProviderView{},
		CapabilityConnections: []DomainCapabilityConnectionView{},
		Cases:                 []DomainCaseView{},
		CaseEvents:            []DomainCaseEventView{},
		Operations:            []DomainOperationView{},
	}
}

// domainCounts 用各数组长度生成 counts；键名与 DomainSnapshot 的数组字段一致，
// 必定覆盖契约要求的 assets/facts/relations/incidents/tickets/loans/
// fault_segments/delivery_records/capabilities/cases/operations/leases。
func domainCounts(s *DomainSnapshot) map[string]int {
	return map[string]int{
		"assets":                 len(s.Assets),
		"facts":                  len(s.Facts),
		"relations":              len(s.Relations),
		"components":             len(s.Components),
		"asset_transitions":      len(s.AssetTransitions),
		"acceptance_coverage":    len(s.AcceptanceCoverage),
		"object_types":           len(s.ObjectTypes),
		"relation_types":         len(s.RelationTypes),
		"observations":           len(s.Observations),
		"fault_segments":         len(s.FaultSegments),
		"self_heal_policies":     len(s.SelfHealPolicies),
		"self_heal_executions":   len(s.SelfHealExecutions),
		"incidents":              len(s.Incidents),
		"hypotheses":             len(s.Hypotheses),
		"proposals":              len(s.Proposals),
		"tickets":                len(s.Tickets),
		"delivery_records":       len(s.DeliveryRecords),
		"delivery_stage_events":  len(s.DeliveryStageEvents),
		"deployment_plans":       len(s.DeploymentPlans),
		"loans":                  len(s.Loans),
		"handovers":              len(s.Handovers),
		"loan_exceptions":        len(s.LoanExceptions),
		"leases":                 len(s.Leases),
		"workers":                len(s.Workers),
		"authorizations":         len(s.Authorizations),
		"domain_packages":        len(s.DomainPackages),
		"retention_policies":     len(s.RetentionPolicies),
		"cleanup_queue":          len(s.CleanupQueue),
		"capabilities":           len(s.Capabilities),
		"capability_providers":   len(s.CapabilityProviders),
		"capability_connections": len(s.CapabilityConnections),
		"cases":                  len(s.Cases),
		"case_events":            len(s.CaseEvents),
		"operations":             len(s.Operations),
	}
}

// domainQueryRows 执行一条只读查询并把每行交给 scan。
// Store.db 是单写连接，必须在下一次 QueryContext 前完整 Close rows。
func domainQueryRows(ctx context.Context, db *sql.DB, label, query string, scan func(*sql.Rows) error) error {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("domain snapshot %s: %w", label, err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return fmt.Errorf("domain snapshot %s: %w", label, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("domain snapshot %s: %w", label, err)
	}
	return nil
}

type domainQuery struct {
	label string
	query string
	scan  func(*sql.Rows) error
}

// DomainSnapshot 读取 V3 领域台账的全部只读视图，返回一份稳定排序的快照。
func (s *Store) DomainSnapshot(ctx context.Context) (DomainSnapshot, error) {
	snap := newDomainSnapshot()

	steps := []domainQuery{
		// ops_assets：健康/可达性/业务占用取每个对象最新一条 asset_transitions，
		// 没有迁移记录时回落到 unknown / 空串。
		{"assets", `SELECT a.object_ref, a.kind, a.identity, a.state, a.revision,
			a.ownership_epoch, a.frozen_at, a.frozen_by, a.freeze_reason,
			a.current_owner, a.created_at, a.updated_at,
			COALESCE((SELECT t.health FROM asset_transitions t WHERE t.object_ref = a.object_ref
				ORDER BY t.occurred_at DESC, t.created_at DESC, t.transition_id DESC LIMIT 1), 'unknown'),
			COALESCE((SELECT t.reachability FROM asset_transitions t WHERE t.object_ref = a.object_ref
				ORDER BY t.occurred_at DESC, t.created_at DESC, t.transition_id DESC LIMIT 1), 'unknown'),
			COALESCE((SELECT t.business_occupancy FROM asset_transitions t WHERE t.object_ref = a.object_ref
				ORDER BY t.occurred_at DESC, t.created_at DESC, t.transition_id DESC LIMIT 1), '')
			FROM ops_assets a ORDER BY a.object_ref`, func(r *sql.Rows) error {
			var v DomainAssetView
			if err := r.Scan(&v.ObjectRef, &v.Kind, &v.Identity, &v.State, &v.Revision,
				&v.OwnershipEpoch, &v.FrozenAt, &v.FrozenBy, &v.FreezeReason,
				&v.CurrentOwner, &v.CreatedAt, &v.UpdatedAt,
				&v.Health, &v.Reachability, &v.BusinessOccupancy); err != nil {
				return err
			}
			snap.Assets = append(snap.Assets, v)
			return nil
		}},
		{"facts", `SELECT fact_id, object_ref, field_key, value, unit, source_ref, authority,
			valid_from, valid_to, recorded_at, revision,
			COALESCE(supersedes, ''), COALESCE(conflict_with, ''), conflict_basis, created_at
			FROM facts ORDER BY object_ref, field_key, valid_from, fact_id`, func(r *sql.Rows) error {
			var v DomainFactView
			if err := r.Scan(&v.FactID, &v.ObjectRef, &v.FieldKey, &v.Value, &v.Unit,
				&v.SourceRef, &v.Authority, &v.ValidFrom, &v.ValidTo, &v.RecordedAt,
				&v.Revision, &v.Supersedes, &v.ConflictWith, &v.ConflictBasis, &v.CreatedAt); err != nil {
				return err
			}
			snap.Facts = append(snap.Facts, v)
			return nil
		}},
		{"relations", `SELECT relation_id, relation_kind, source_ref, target_ref,
			valid_from, valid_to, created_at FROM relations ORDER BY relation_id`, func(r *sql.Rows) error {
			var v DomainRelationView
			if err := r.Scan(&v.RelationID, &v.RelationKind, &v.SourceRef, &v.TargetRef,
				&v.ValidFrom, &v.ValidTo, &v.CreatedAt); err != nil {
				return err
			}
			snap.Relations = append(snap.Relations, v)
			return nil
		}},
		{"components", `SELECT installation_id, node_ref, component_ref, component_kind,
			installed_at, removed_at, node_identity_continuous, created_at
			FROM component_installations ORDER BY installation_id`, func(r *sql.Rows) error {
			var v DomainComponentView
			if err := r.Scan(&v.InstallationID, &v.NodeRef, &v.ComponentRef, &v.ComponentKind,
				&v.InstalledAt, &v.RemovedAt, &v.NodeIdentityContinuous, &v.CreatedAt); err != nil {
				return err
			}
			snap.Components = append(snap.Components, v)
			return nil
		}},
		{"asset_transitions", `SELECT transition_id, object_ref, from_state, to_state,
			trigger_kind, evidence_refs, rule_refs, expected_revision, expected_ownership_epoch,
			actor, responsibility, health, reachability, business_occupancy, occurred_at, created_at
			FROM asset_transitions ORDER BY object_ref, occurred_at, transition_id`, func(r *sql.Rows) error {
			var v DomainTransitionView
			if err := r.Scan(&v.TransitionID, &v.ObjectRef, &v.FromState, &v.ToState,
				&v.TriggerKind, &v.EvidenceRefs, &v.RuleRefs, &v.ExpectedRevision,
				&v.ExpectedOwnershipEpoch, &v.Actor, &v.Responsibility, &v.Health,
				&v.Reachability, &v.BusinessOccupancy, &v.OccurredAt, &v.CreatedAt); err != nil {
				return err
			}
			snap.AssetTransitions = append(snap.AssetTransitions, v)
			return nil
		}},
		{"acceptance_coverage", `SELECT coverage_id, requirement_key, object_ref, coverage_state,
			not_applicable_basis, COALESCE(validation_ref, ''), created_at, updated_at
			FROM acceptance_coverage ORDER BY coverage_id`, func(r *sql.Rows) error {
			var v DomainCoverageView
			if err := r.Scan(&v.CoverageID, &v.RequirementKey, &v.ObjectRef, &v.CoverageState,
				&v.NotApplicableBasis, &v.ValidationRef, &v.CreatedAt, &v.UpdatedAt); err != nil {
				return err
			}
			snap.AcceptanceCoverage = append(snap.AcceptanceCoverage, v)
			return nil
		}},
		{"object_types", `SELECT package_id, type_key, fields, cardinality, identity_rule,
			data_binding, config_spec, test_spec, created_at
			FROM domain_object_types ORDER BY package_id, type_key`, func(r *sql.Rows) error {
			var v DomainObjectTypeView
			if err := r.Scan(&v.PackageID, &v.TypeKey, &v.Fields, &v.Cardinality,
				&v.IdentityRule, &v.DataBinding, &v.ConfigSpec, &v.TestSpec, &v.CreatedAt); err != nil {
				return err
			}
			snap.ObjectTypes = append(snap.ObjectTypes, v)
			return nil
		}},
		{"relation_types", `SELECT package_id, relation_key, source_type, target_type,
			cardinality, mutually_exclusive_with, created_at
			FROM domain_relation_types ORDER BY package_id, relation_key`, func(r *sql.Rows) error {
			var v DomainRelationTypeView
			if err := r.Scan(&v.PackageID, &v.RelationKey, &v.SourceType, &v.TargetType,
				&v.Cardinality, &v.MutuallyExclusiveWith, &v.CreatedAt); err != nil {
				return err
			}
			snap.RelationTypes = append(snap.RelationTypes, v)
			return nil
		}},
		{"observations", `SELECT observation_id, source_ref, source_identity, collected_at,
			coverage, object_ref, field_key, value, unit, recorded_at
			FROM observations ORDER BY observation_id`, func(r *sql.Rows) error {
			var v DomainObservationView
			if err := r.Scan(&v.ObservationID, &v.SourceRef, &v.SourceIdentity, &v.CollectedAt,
				&v.Coverage, &v.ObjectRef, &v.FieldKey, &v.Value, &v.Unit, &v.RecordedAt); err != nil {
				return err
			}
			snap.Observations = append(snap.Observations, v)
			return nil
		}},
		{"fault_segments", `SELECT segment_id, fault_key, observation_refs, window_start,
			window_end, state, created_at, updated_at FROM fault_segments ORDER BY segment_id`, func(r *sql.Rows) error {
			var v DomainFaultSegmentView
			if err := r.Scan(&v.SegmentID, &v.FaultKey, &v.ObservationRefs, &v.WindowStart,
				&v.WindowEnd, &v.State, &v.CreatedAt, &v.UpdatedAt); err != nil {
				return err
			}
			snap.FaultSegments = append(snap.FaultSegments, v)
			return nil
		}},
		{"self_heal_policies", `SELECT policy_id, policy_key, version, subject, asset_scope,
			valid_from, valid_until, preconditions, action_capability, verification_standard,
			per_node_window_ms, per_node_max_writes, cluster_max_concurrent, state, observe_only,
			published_by, published_at, created_at, updated_at
			FROM self_heal_policies ORDER BY policy_id`, func(r *sql.Rows) error {
			var v DomainSelfHealPolicyView
			if err := r.Scan(&v.PolicyID, &v.PolicyKey, &v.Version, &v.Subject, &v.AssetScope,
				&v.ValidFrom, &v.ValidUntil, &v.Preconditions, &v.ActionCapability,
				&v.VerificationStandard, &v.PerNodeWindowMS, &v.PerNodeMaxWrites,
				&v.ClusterMaxConcurrent, &v.State, &v.ObserveOnly, &v.PublishedBy,
				&v.PublishedAt, &v.CreatedAt, &v.UpdatedAt); err != nil {
				return err
			}
			snap.SelfHealPolicies = append(snap.SelfHealPolicies, v)
			return nil
		}},
		{"self_heal_executions", `SELECT execution_id, policy_id, object_ref,
			COALESCE(operation_id, ''), state, block_reason, verification_outcome,
			created_at, updated_at FROM self_heal_executions ORDER BY execution_id`, func(r *sql.Rows) error {
			var v DomainSelfHealExecutionView
			if err := r.Scan(&v.ExecutionID, &v.PolicyID, &v.ObjectRef, &v.OperationID,
				&v.State, &v.BlockReason, &v.VerificationOutcome, &v.CreatedAt, &v.UpdatedAt); err != nil {
				return err
			}
			snap.SelfHealExecutions = append(snap.SelfHealExecutions, v)
			return nil
		}},
		{"incidents", `SELECT incident_id, symptoms, impact_scope, timeline, state,
			recovery_verified, recovery_evidence, root_cause_proven, root_cause,
			root_cause_evidence, created_at, updated_at FROM incidents ORDER BY incident_id`, func(r *sql.Rows) error {
			var v DomainIncidentView
			if err := r.Scan(&v.IncidentID, &v.Symptoms, &v.ImpactScope, &v.Timeline,
				&v.State, &v.RecoveryVerified, &v.RecoveryEvidence, &v.RootCauseProven,
				&v.RootCause, &v.RootCauseEvidence, &v.CreatedAt, &v.UpdatedAt); err != nil {
				return err
			}
			snap.Incidents = append(snap.Incidents, v)
			return nil
		}},
		{"hypotheses", `SELECT hypothesis_id, incident_id, statement, proposed_by,
			supporting_evidence, counter_evidence, state, unknowns, next_queries,
			created_at, updated_at FROM incident_hypotheses ORDER BY hypothesis_id`, func(r *sql.Rows) error {
			var v DomainHypothesisView
			if err := r.Scan(&v.HypothesisID, &v.IncidentID, &v.Statement, &v.ProposedBy,
				&v.SupportingEvidence, &v.CounterEvidence, &v.State, &v.Unknowns,
				&v.NextQueries, &v.CreatedAt, &v.UpdatedAt); err != nil {
				return err
			}
			snap.Hypotheses = append(snap.Hypotheses, v)
			return nil
		}},
		{"proposals", `SELECT proposal_id, incident_id, object_refs, capability_key, params,
			evidence_refs, risk_boundary, state, proposed_by, created_at
			FROM incident_proposals ORDER BY proposal_id`, func(r *sql.Rows) error {
			var v DomainProposalView
			if err := r.Scan(&v.ProposalID, &v.IncidentID, &v.ObjectRefs, &v.CapabilityKey,
				&v.Params, &v.EvidenceRefs, &v.RiskBoundary, &v.State, &v.ProposedBy,
				&v.CreatedAt); err != nil {
				return err
			}
			snap.Proposals = append(snap.Proposals, v)
			return nil
		}},
		{"tickets", `SELECT ticket_id, external_system, external_ticket_id,
			COALESCE(incident_id, ''), COALESCE(takeover_id, ''), assignee, object_ref,
			component_refs, fault_evidence, external_state_version, external_state, state,
			verification_outcome, residual_limitations, follow_up_responsibility,
			created_at, updated_at FROM tickets ORDER BY ticket_id`, func(r *sql.Rows) error {
			var v DomainTicketView
			if err := r.Scan(&v.TicketID, &v.ExternalSystem, &v.ExternalTicketID,
				&v.IncidentID, &v.TakeoverID, &v.Assignee, &v.ObjectRef, &v.ComponentRefs,
				&v.FaultEvidence, &v.ExternalStateVersion, &v.ExternalState, &v.State,
				&v.VerificationOutcome, &v.ResidualLimitations, &v.FollowUpResponsibility,
				&v.CreatedAt, &v.UpdatedAt); err != nil {
				return err
			}
			snap.Tickets = append(snap.Tickets, v)
			return nil
		}},
		{"delivery_records", `SELECT delivery_id, object_ref, stage, record_ref, evidence_refs,
			occurred_at, created_at FROM delivery_records
			ORDER BY object_ref, occurred_at, delivery_id`, func(r *sql.Rows) error {
			var v DomainDeliveryRecordView
			if err := r.Scan(&v.DeliveryID, &v.ObjectRef, &v.Stage, &v.RecordRef,
				&v.EvidenceRefs, &v.OccurredAt, &v.CreatedAt); err != nil {
				return err
			}
			snap.DeliveryRecords = append(snap.DeliveryRecords, v)
			return nil
		}},
		// deployment_plans：仓库里没有独立表，runtime_deployment.go 把计划登记成
		// idempotency_key='deploy-<planID>' 的 Operation，见 restoreDeploymentPlan。
		{"delivery_stage_events", `SELECT event_id, object_ref, stage, record_ref, evidence_refs,
			occurred_at, created_at FROM delivery_stage_events ORDER BY object_ref, occurred_at, event_id`, func(r *sql.Rows) error {
			var v DomainDeliveryStageEventView
			if err := r.Scan(&v.EventID, &v.ObjectRef, &v.Stage, &v.RecordRef, &v.EvidenceRefs, &v.OccurredAt, &v.CreatedAt); err != nil {
				return err
			}
			snap.DeliveryStageEvents = append(snap.DeliveryStageEvents, v)
			return nil
		}},
		{"deployment_plans", `SELECT idempotency_key, target_refs, state, registered_at, updated_at
			FROM operations WHERE idempotency_key LIKE 'deploy-%' ORDER BY idempotency_key`, func(r *sql.Rows) error {
			var key, targetRefs, state string
			var createdAt, updatedAt int64
			if err := r.Scan(&key, &targetRefs, &state, &createdAt, &updatedAt); err != nil {
				return err
			}
			snap.DeploymentPlans = append(snap.DeploymentPlans,
				restoreDeploymentPlan(strings.TrimPrefix(key, "deploy-"), targetRefs, state, createdAt, updatedAt))
			return nil
		}},
		{"loans", `SELECT loan_id, object_ref, handover_id, lender, borrower, due_at,
			read_only_scope, state, ownership_epoch_at_lend, lent_at, returned_at
			FROM loans ORDER BY loan_id`, func(r *sql.Rows) error {
			var v DomainLoanView
			if err := r.Scan(&v.LoanID, &v.ObjectRef, &v.HandoverID, &v.Lender, &v.Borrower,
				&v.DueAt, &v.ReadOnlyScope, &v.State, &v.OwnershipEpochAtLend,
				&v.LentAt, &v.ReturnedAt); err != nil {
				return err
			}
			snap.Loans = append(snap.Loans, v)
			return nil
		}},
		{"handovers", `SELECT handover_id, object_ref, intent, frozen_at, ownership_epoch_before,
			conflict_writes, occupancy, responsibility, lender, borrower, due_at,
			read_only_scope, exception_conditions, state, created_at, updated_at
			FROM loan_handovers ORDER BY handover_id`, func(r *sql.Rows) error {
			var v DomainHandoverView
			if err := r.Scan(&v.HandoverID, &v.ObjectRef, &v.Intent, &v.FrozenAt,
				&v.OwnershipEpochBefore, &v.ConflictWrites, &v.Occupancy, &v.Responsibility,
				&v.Lender, &v.Borrower, &v.DueAt, &v.ReadOnlyScope, &v.ExceptionConditions,
				&v.State, &v.CreatedAt, &v.UpdatedAt); err != nil {
				return err
			}
			snap.Handovers = append(snap.Handovers, v)
			return nil
		}},
		{"loan_exceptions", `SELECT exception_id, handover_id, object_ref, authorized_by,
			received_by, unknown_effects, follow_up_responsibility, created_at
			FROM loan_exceptions ORDER BY exception_id`, func(r *sql.Rows) error {
			var v DomainLoanExceptionView
			if err := r.Scan(&v.ExceptionID, &v.HandoverID, &v.ObjectRef, &v.AuthorizedBy,
				&v.ReceivedBy, &v.UnknownEffects, &v.FollowUpResponsibility, &v.CreatedAt); err != nil {
				return err
			}
			snap.LoanExceptions = append(snap.LoanExceptions, v)
			return nil
		}},
		{"leases", `SELECT lease_id, unit_id, task_id, lease_generation, worker_id, token_id,
			issued_at, lease_until, state, reclaim_reason, reclaimed_at, dispatched_ops
			FROM work_leases ORDER BY lease_id`, func(r *sql.Rows) error {
			var v DomainLeaseView
			if err := r.Scan(&v.LeaseID, &v.UnitID, &v.TaskID, &v.LeaseGeneration,
				&v.WorkerID, &v.TokenID, &v.IssuedAt, &v.LeaseUntil, &v.State,
				&v.ReclaimReason, &v.ReclaimedAt, &v.DispatchedOps); err != nil {
				return err
			}
			snap.Leases = append(snap.Leases, v)
			return nil
		}},
		// worker_instances 的真实列名是 last_contact_at，契约字段为 last_seen_at。
		{"workers", `SELECT worker_id, role, supported_kinds, instance_version,
			last_contact_at AS last_seen_at, enabled, revoked_at, expires_at, created_at
			FROM worker_instances ORDER BY worker_id`, func(r *sql.Rows) error {
			var v DomainWorkerView
			if err := r.Scan(&v.WorkerID, &v.Role, &v.SupportedKinds, &v.InstanceVersion,
				&v.LastSeenAt, &v.Enabled, &v.RevokedAt, &v.ExpiresAt, &v.CreatedAt); err != nil {
				return err
			}
			snap.Workers = append(snap.Workers, v)
			return nil
		}},
		{"authorizations", `SELECT auth_id, subject, action, object_ref, scope_label,
			param_constraints, source, policy_version, valid_from, expires_at, revoked_at,
			revoked_by, owner, revision, created_at, updated_at
			FROM authorizations ORDER BY auth_id`, func(r *sql.Rows) error {
			var v DomainAuthorizationView
			if err := r.Scan(&v.AuthID, &v.Subject, &v.Action, &v.ObjectRef, &v.ScopeLabel,
				&v.ParamConstraints, &v.Source, &v.PolicyVersion, &v.ValidFrom, &v.ExpiresAt,
				&v.RevokedAt, &v.RevokedBy, &v.Owner, &v.Revision, &v.CreatedAt, &v.UpdatedAt); err != nil {
				return err
			}
			snap.Authorizations = append(snap.Authorizations, v)
			return nil
		}},
		{"domain_packages", `SELECT package_id, name, version, state, definitions,
			definitions_digest, compatibility, impact_summary, rollback_basis,
			published_by, published_at, created_at, updated_at
			FROM domain_packages ORDER BY package_id`, func(r *sql.Rows) error {
			var v DomainPackageView
			if err := r.Scan(&v.PackageID, &v.Name, &v.Version, &v.State, &v.Definitions,
				&v.DefinitionsDigest, &v.Compatibility, &v.ImpactSummary, &v.RollbackBasis,
				&v.PublishedBy, &v.PublishedAt, &v.CreatedAt, &v.UpdatedAt); err != nil {
				return err
			}
			snap.DomainPackages = append(snap.DomainPackages, v)
			return nil
		}},
		{"retention_policies", `SELECT policy_key, object_kind, retention_ms, legal_hold,
			business_hold, tombstone_ms, unknown_retention_ms, created_at, updated_at
			FROM retention_policies ORDER BY policy_key`, func(r *sql.Rows) error {
			var v DomainRetentionPolicyView
			if err := r.Scan(&v.PolicyKey, &v.ObjectKind, &v.RetentionMS, &v.LegalHold,
				&v.BusinessHold, &v.TombstoneMS, &v.UnknownRetentionMS, &v.CreatedAt,
				&v.UpdatedAt); err != nil {
				return err
			}
			snap.RetentionPolicies = append(snap.RetentionPolicies, v)
			return nil
		}},
		{"cleanup_queue", `SELECT cleanup_id, kind, object_ref, source_ref, unreadable,
			source_version, policy_version, state, retry_count, next_retry_at,
			failure_reason, created_at, updated_at FROM cleanup_queue ORDER BY cleanup_id`, func(r *sql.Rows) error {
			var v DomainCleanupItemView
			if err := r.Scan(&v.CleanupID, &v.Kind, &v.ObjectRef, &v.SourceRef, &v.Unreadable,
				&v.SourceVersion, &v.PolicyVersion, &v.State, &v.RetryCount, &v.NextRetryAt,
				&v.FailureReason, &v.CreatedAt, &v.UpdatedAt); err != nil {
				return err
			}
			snap.CleanupQueue = append(snap.CleanupQueue, v)
			return nil
		}},
		{"capabilities", `SELECT capability_id, capability_key, version, kind, input_schema,
			output_schema, target_kind, execution_mode, supports_get, supports_cancel,
			supports_events, supports_materials, idempotent, fencing, verified_scope,
			exposed_to_model, created_at, updated_at FROM capabilities ORDER BY capability_id`, func(r *sql.Rows) error {
			var v DomainCapabilityView
			if err := r.Scan(&v.CapabilityID, &v.CapabilityKey, &v.Version, &v.Kind,
				&v.InputSchema, &v.OutputSchema, &v.TargetKind, &v.ExecutionMode,
				&v.SupportsGet, &v.SupportsCancel, &v.SupportsEvents, &v.SupportsMaterials,
				&v.Idempotent, &v.Fencing, &v.VerifiedScope, &v.ExposedToModel,
				&v.CreatedAt, &v.UpdatedAt); err != nil {
				return err
			}
			snap.Capabilities = append(snap.Capabilities, v)
			return nil
		}},
		{"capability_providers", `SELECT provider_id, provider_key, version, kind, config_ref,
			capabilities, created_at FROM capability_providers ORDER BY provider_id`, func(r *sql.Rows) error {
			var v DomainProviderView
			if err := r.Scan(&v.ProviderID, &v.ProviderKey, &v.Version, &v.Kind,
				&v.ConfigRef, &v.Capabilities, &v.CreatedAt); err != nil {
				return err
			}
			snap.CapabilityProviders = append(snap.CapabilityProviders, v)
			return nil
		}},
		{"capability_connections", `SELECT connection_id, provider_id, connection_name,
			connection_version, credential_ref, enabled, created_at
			FROM capability_connections ORDER BY connection_id`, func(r *sql.Rows) error {
			var v DomainCapabilityConnectionView
			if err := r.Scan(&v.ConnectionID, &v.ProviderID, &v.ConnectionName,
				&v.ConnectionVersion, &v.CredentialRef, &v.Enabled, &v.CreatedAt); err != nil {
				return err
			}
			snap.CapabilityConnections = append(snap.CapabilityConnections, v)
			return nil
		}},
		{"cases", `SELECT case_id, symptom, object_ref, environment, hypothesis, actual_action,
			independent_outcome, unknowns, source_refs, applicability, contraindications,
			invalidation_conditions, counterexamples, evidence_refs, review_at, root_cause,
			root_cause_proven, state, version, COALESCE(supersedes, ''), superseded_reason,
			published_by, published_at, created_at, updated_at
			FROM operational_cases ORDER BY case_id`, func(r *sql.Rows) error {
			var v DomainCaseView
			if err := r.Scan(&v.CaseID, &v.Symptom, &v.ObjectRef, &v.Environment,
				&v.Hypothesis, &v.ActualAction, &v.IndependentOutcome, &v.Unknowns,
				&v.SourceRefs, &v.Applicability, &v.Contraindications,
				&v.InvalidationConditions, &v.Counterexamples, &v.EvidenceRefs,
				&v.ReviewAt, &v.RootCause, &v.RootCauseProven, &v.State, &v.Version,
				&v.Supersedes, &v.SupersededReason, &v.PublishedBy, &v.PublishedAt,
				&v.CreatedAt, &v.UpdatedAt); err != nil {
				return err
			}
			snap.Cases = append(snap.Cases, v)
			return nil
		}},
		{"case_events", `SELECT event_id, case_id, kind, task_id, object_ref, evidence_ref,
			detail, occurred_at FROM case_events ORDER BY event_id`, func(r *sql.Rows) error {
			var v DomainCaseEventView
			if err := r.Scan(&v.EventID, &v.CaseID, &v.Kind, &v.TaskID, &v.ObjectRef,
				&v.EvidenceRef, &v.Detail, &v.OccurredAt); err != nil {
				return err
			}
			snap.CaseEvents = append(snap.CaseEvents, v)
			return nil
		}},
		{"operations", `SELECT operation_id, task_id, COALESCE(step_id, ''), capability_key,
			capability_version, connection_id, target_refs, params_digest, authorization_ref,
			idempotency_key, state, attempt_count, COALESCE(current_attempt_id, ''),
			COALESCE(predecessor_id, ''), registered_at, updated_at, completed_at,
			effect_refs, failure_reason FROM operations ORDER BY operation_id`, func(r *sql.Rows) error {
			var v DomainOperationView
			if err := r.Scan(&v.OperationID, &v.TaskID, &v.StepID, &v.CapabilityKey,
				&v.CapabilityVersion, &v.ConnectionID, &v.TargetRefs, &v.ParamsDigest,
				&v.AuthorizationRef, &v.IdempotencyKey, &v.State, &v.AttemptCount,
				&v.CurrentAttemptID, &v.PredecessorID, &v.RegisteredAt, &v.UpdatedAt,
				&v.CompletedAt, &v.EffectRefs, &v.FailureReason); err != nil {
				return err
			}
			snap.Operations = append(snap.Operations, v)
			return nil
		}},
	}

	for _, step := range steps {
		if err := domainQueryRows(ctx, s.db, step.label, step.query, step.scan); err != nil {
			return DomainSnapshot{}, err
		}
	}
	snap.Counts = domainCounts(&snap)
	return snap, nil
}

// restoreDeploymentPlan 从 idempotency_key='deploy-<planID>' 的 Operation 还原部署计划。
//
// 仓库没有 deployment_plans 独立表；runtime_deployment.go 的 createDeploymentPlan
// 只把 p.Targets 序列化进 operations.target_refs，Batches 以 json.Marshal 后的
// SHA-256 摘要保存在 params_digest，ImageRef / DeployMethod 根本不落库。
// 因此 image_ref / deploy_method / batches 无法从持久数据无损还原：这里尽力解析
// target_refs（兼容额外携带这些键的扩展 JSON 对象），解析不到就给合理空值
// （image_ref=”、deploy_method=”、batches='[]'），不编造不存在的字段值；
// targets 保持原始 JSON 字符串，state 用 operation.state，时间用 operations 的时间列。
func restoreDeploymentPlan(planID, targetRefs, state string, createdAt, updatedAt int64) DomainDeploymentPlanView {
	plan := DomainDeploymentPlanView{
		PlanID:       planID,
		ImageRef:     "",
		DeployMethod: "",
		State:        state,
		Batches:      "[]",
		Targets:      "[]",
		CreatedAt:    createdAt,
		UpdatedAt:    updatedAt,
	}
	raw := strings.TrimSpace(targetRefs)
	if raw != "" {
		plan.Targets = raw
	}
	// 常规格式是 DeploymentTarget 数组；若扩展实现把计划完整写进 target_refs，
	// 则把能解析的字段逐个取出，其余保持空值。
	var envelope struct {
		ImageRef     string          `json:"image_ref"`
		DeployMethod string          `json:"deploy_method"`
		Batches      json.RawMessage `json:"batches"`
		Targets      json.RawMessage `json:"targets"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err == nil &&
		(envelope.ImageRef != "" || envelope.DeployMethod != "" ||
			len(envelope.Batches) > 0 || len(envelope.Targets) > 0) {
		plan.ImageRef = envelope.ImageRef
		plan.DeployMethod = envelope.DeployMethod
		if len(envelope.Batches) > 0 {
			plan.Batches = string(envelope.Batches)
		}
		if len(envelope.Targets) > 0 {
			plan.Targets = string(envelope.Targets)
		}
	}
	return plan
}
